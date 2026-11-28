// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package redupe

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dgraph-io/badger/v4"
	"github.com/nationallibraryofnorway/gowarc/v4"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/diskspace"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcdigest"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type missingTarget struct{ link *entry }

func (e *missingTarget) Error() string { return "missing revisit target" }
func (e *missingTarget) target() string {
	c := e.link.Capture
	if c.GetRoi() != "" {
		return c.GetRoi()
	}
	return c.GetRou() + " " + c.GetRod().AsTime().UTC().Format("2006-01-02T15:04:05.999999999Z")
}

type auditEvidence struct {
	ResponseFramingError string            `json:"responseFramingError,omitempty"`
	ResponseEntityLength *int64            `json:"responseEntityLength,omitzero"`
	ResourceFramingError string            `json:"resourceFramingError,omitempty"`
	ResourceEntityLength *int64            `json:"resourceEntityLength,omitzero"`
	Stored               map[string]string `json:"storedBody,omitempty"`
	BlockVerified        bool              `json:"blockVerified,omitzero"`
	Header               string            `json:"header,omitempty"`
	Payload              map[string]string `json:"payload,omitempty"`
	Declared             string            `json:"declared,omitempty"`
	Issue                string            `json:"issue,omitempty"`
}

func measureRecord(r gowarc.WarcRecord) (*auditEvidence, error) {
	ev := &auditEvidence{Declared: r.WarcHeader().Get(gowarc.WarcPayloadDigest)}
	if block, ok := r.Block().(gowarc.ProtocolHeaderBlock); ok && len(block.ProtocolHeaderBytes()) > 0 {
		ev.Header = hashString(string(block.ProtocolHeaderBytes()))
	}
	if d := r.WarcHeader().Get(gowarc.WarcBlockDigest); d != "" {
		ev.BlockVerified = warcdigest.Equal(d, r.Block().BlockDigest())
		if !ev.BlockVerified {
			ev.Issue = "block digest mismatch"
		}
	}
	if r.Type() == gowarc.Revisit {
		if n, err := loader.ResponseRevisitLength(r); err != nil {
			ev.ResponseFramingError = err.Error()
		} else if n >= 0 {
			ev.ResponseEntityLength = &n
		}
		framing, err := loader.ParseResourceRevisitFraming(r)
		if err != nil {
			ev.ResourceFramingError = err.Error()
		} else if framing.ContentLength >= 0 {
			ev.ResourceEntityLength = &framing.ContentLength
		}
	}
	if r.Type() != gowarc.Response && r.Type() != gowarc.Resource {
		return ev, nil
	}
	if r.Type() == gowarc.Response {
		if _, ok := r.Block().(gowarc.HttpResponseBlock); !ok {
			ev.Issue = "unsupported response payload boundary"
			return ev, nil
		}
	}
	raw, err := payloadReader(r)
	if err != nil {
		return nil, err
	}
	ev.Payload, err = auditDigests(raw)
	if err != nil {
		return nil, err
	}
	if b, ok := r.Block().(gowarc.HttpBlock); ok && b.PayloadLength() >= 0 && b.PayloadLength() < b.Size()-int64(len(b.ProtocolHeaderBytes())) {
		stored, err := b.RawBytes()
		if err != nil {
			return nil, err
		}
		if _, err = io.CopyN(io.Discard, stored, int64(len(b.ProtocolHeaderBytes()))); err != nil {
			return nil, err
		}
		ev.Stored, err = auditDigests(stored)
		if err != nil {
			return nil, err
		}
	}
	if ev.Issue == "" && ev.Declared != "" && !ev.matches(ev.Declared) {
		ev.Issue = "donor payload digest mismatch"
	}
	return ev, nil
}
func auditDigests(raw io.Reader) (map[string]string, error) {
	m, a, b, c := md5.New(), sha1.New(), sha256.New(), sha512.New()
	if _, err := io.Copy(io.MultiWriter(m, a, b, c), raw); err != nil {
		return nil, err
	}
	return map[string]string{"md5": hex.EncodeToString(m.Sum(nil)), "sha1": hex.EncodeToString(a.Sum(nil)), "sha256": hex.EncodeToString(b.Sum(nil)), "sha512": hex.EncodeToString(c.Sum(nil))}, nil
}
func (e *auditEvidence) matches(label string) bool {
	return matchesDigest(e.Payload, label)
}
func matchesDigest(values map[string]string, label string) bool {
	alg, b, err := warcdigest.Parse(label)
	return err == nil && values[alg] == hex.EncodeToString(b)
}

type auditFinding struct {
	Digest    string `json:"digest,omitempty"`
	NewOffset *int64 `json:"newOffset,omitempty"`
	Status    string `json:"status"`
	File      string `json:"file"`
	Offset    int64  `json:"offset"`
	ID        string `json:"recordId,omitempty"`
	Target    string `json:"target,omitempty"`
	Detail    string `json:"detail,omitempty"`
}
type auditSummary struct {
	Destination string         `json:"recoveryDirectory,omitempty"`
	Verified    *auditSummary  `json:"afterRecovery,omitempty"`
	Files       int            `json:"filesExamined"`
	Revisits    int            `json:"revisitsExamined"`
	Missing     int            `json:"distinctMissingTargets"`
	Complete    bool           `json:"scanComplete"`
	Counts      map[string]int `json:"counts"`
}
type collection struct {
	*RedupeOptions
	summary auditSummary
	failed  bool
}

func newCollection(work string) (*collection, error) {
	db, err := badger.Open(badger.DefaultOptions(work).WithLogger(nil))
	if err != nil {
		return nil, err
	}
	o := &RedupeOptions{scratch: db, collectionAudit: true}
	o.catalog = &inventoryCatalog{o: o}
	return &collection{RedupeOptions: o, summary: auditSummary{Complete: true, Counts: map[string]int{}}}, nil
}
func (a *collection) finding(f auditFinding) error {
	a.summary.Counts[f.Status]++
	key := fmt.Sprintf("x/audit-finding/%s/%s/%020d/%s", hashString(f.Target), hashString(f.File), f.Offset, f.Status)
	return a.put(key, f)
}
func warcPath(p string) bool {
	p = strings.ToLower(p)
	return strings.HasSuffix(p, ".warc") || strings.HasSuffix(p, ".warc.gz")
}
func (a *collection) inventoryRoot(ctx context.Context, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if walkErr == nil {
			if d.Type()&os.ModeSymlink != 0 {
				info, err := os.Stat(path)
				if err != nil || info.IsDir() {
					a.failed = true
					a.summary.Complete = false
					return a.finding(auditFinding{Status: "unreadable", File: rel, Detail: "directory or broken symlink excluded"})
				}
			}
			if d.IsDir() || !warcPath(path) {
				return nil
			}
			a.summary.Files++
			if !d.Type().IsRegular() {
				walkErr = errors.New("archive is not a regular file")
			} else {
				_, walkErr = a.inventoryFile(ctx, afero.NewOsFs(), path, func(e *entry) error {
					if err := a.put("x/offset/"+hashString(rel)+fmt.Sprintf("/%020d", e.Capture.GetOffset()), e.Capture.GetRef()); err != nil {
						return err
					}
					return a.put("x/local/"+hashString(e.Capture.GetSource()), rel)
				})
			}
		}
		if diskspace.Exhausted(walkErr) {
			return fmt.Errorf("%s: %w", path, walkErr)
		}
		if walkErr != nil {
			a.failed = true
			a.summary.Complete = false
			return a.finding(auditFinding{Status: "unreadable", File: rel, Detail: walkErr.Error()})
		}
		return nil
	})
}
func (a *collection) byID(ctx context.Context, id string) (*entry, error) {
	var found *entry
	err := a.catalog.CapturesByID(ctx, id, func(c *cdx.Capture) error {
		e, err := a.entry(c.GetRef())
		if err != nil {
			return err
		}
		if found != nil && e.Fingerprint != found.Fingerprint {
			return errors.New("conflicting record ID")
		}
		found = e
		return nil
	})
	return found, err
}
func (a *collection) visitRecords(fn func(*entry) error) error {
	return a.scan("x/record/", func(data []byte) error {
		var e entry
		if err := decodeEntry(data, &e); err != nil {
			return err
		}
		return fn(&e)
	})
}
func (a *collection) assess(ctx context.Context, e *entry, originals *collection) (auditFinding, *entry, error) {
	c := e.Capture
	f := auditFinding{ID: c.GetRid(), Offset: c.GetOffset()}
	if err := a.get("x/local/"+hashString(c.GetSource()), &f.File); err != nil {
		return f, nil, err
	}
	_, idErr := a.byID(ctx, c.GetRid())
	if diskspace.Exhausted(idErr) {
		return f, nil, idErr
	}
	if idErr != nil {
		f.Status = "conflicting-id"
		f.Detail = idErr.Error()
		return f, nil, nil
	}
	if c.GetSrt() != actionRevisit {
		if legacyDonor(e) {
			f.Status = "legacy-chunked-digest"
			f.Digest = e.Audit.Declared
		} else if len(e.Audit.Stored) > 0 && (e.Audit.Issue != "" || e.Issue != "") {
			f.Status = "invalid-record"
			f.Detail = strings.TrimSpace(e.Issue + " " + e.Audit.Issue)
		}
		return f, nil, nil
	}
	f.Digest = e.Audit.Declared
	donor, chain, err := a.resolve(ctx, e, map[string]bool{})
	if diskspace.Exhausted(err) {
		return f, nil, err
	}
	var missing *missingTarget
	if errors.As(err, &missing) {
		f.Target = missing.target()
		f.Status = "directly-dangling"
		if missing.link.Capture.GetRef() != c.GetRef() {
			f.Status = "transitively-dangling"
		}
	} else if err != nil {
		f.Status = "unverifiable"
		f.Detail = err.Error()
	} else {
		f.Status = "output-consistent"
		for _, link := range append([]*entry{e}, chain...) {
			if link.Issue != "" || (link.Audit.Issue != "" && !(link == donor && legacyDonor(donor))) {
				f.Status = "invalid-record"
				f.Detail = strings.TrimSpace(link.Issue + " " + link.Audit.Issue)
				break
			}
			if link.Capture.GetSrt() != actionRevisit {
				continue
			}
			switch donor.Capture.GetSrt() {
			case "response":
				if link.Audit.ResponseFramingError != "" {
					f.Status, f.Detail = "unverifiable", link.Audit.ResponseFramingError
				} else if n := link.Audit.ResponseEntityLength; n != nil && *n != donor.Capture.GetPle() {
					f.Status, f.Detail = "unverifiable", fmt.Sprintf("response revisit expects %d payload bytes, donor supplied %d", *n, donor.Capture.GetPle())
				}
			case "resource":
				if link.Audit.ResourceFramingError != "" {
					f.Status, f.Detail = "unverifiable", link.Audit.ResourceFramingError
				} else if n := link.Audit.ResourceEntityLength; n != nil && *n != donor.Capture.GetPle() {
					f.Status, f.Detail = "unverifiable", "resource revisit length differs from donor payload"
				}
			}
			if f.Status == "unverifiable" {
				break
			}
			label := link.Audit.Declared
			if _, _, err := warcdigest.Parse(label); err != nil {
				f.Status = "unverifiable"
				f.Detail = err.Error()
				break
			}
			if !donor.Audit.matches(label) {
				if matchesDigest(donor.Audit.Stored, label) && donor.Audit.Declared != "" {
					f.Status = "legacy-chunked-digest"
					continue
				}
				f.Status = "digest-mismatch"
				f.Detail = link.Capture.GetRid()
				break
			}
		}
	}
	if f.Status == "output-consistent" && legacyDonor(donor) {
		f.Status = "legacy-chunked-digest"
	}
	var original *entry
	if originals != nil {
		original, err = originals.byID(ctx, c.GetRid())
		if diskspace.Exhausted(err) {
			return f, nil, err
		}
		if err != nil {
			f.Status = "unverifiable"
			f.Detail = err.Error()
			return f, nil, nil
		}
		if original != nil {
			oc := original.Capture
			if oc.GetUri() != c.GetUri() || !oc.GetSts().AsTime().Equal(c.GetSts().AsTime()) {
				f.Status = "original-identity-conflict"
				return f, nil, nil
			}
			if oc.GetSrt() == actionRevisit {
				if f.Status == "output-consistent" {
					f.Status = "pre-existing-revisit"
				}
				return f, nil, nil
			}
			if !validFull(original) && !staleOriginal(original) {
				f.Detail = "original cannot be verified"
				if f.Status == "output-consistent" {
					f.Status = "unverifiable"
				}
				return f, nil, nil
			}
			if f.Status == "output-consistent" {
				if original.Audit.Header != e.Audit.Header || oc.GetSrt() != donor.Capture.GetSrt() || original.Audit.Payload["sha256"] != donor.Audit.Payload["sha256"] {
					f.Status = "original-mismatch"
				} else {
					f.Status = "original-verified"
				}
			}
		}
	}
	if original != nil && staleOriginal(original) {
		f.Detail = "original payload header contradicts original bytes; original block digest verified"
	}
	return f, original, nil
}
func good(status string) bool {
	return status == "output-consistent" || status == "original-verified" || status == "pre-existing-revisit"
}
func (a *collection) examine(ctx context.Context, originals *collection, act func(*entry, auditFinding, *entry) error) error {
	return a.visitRecords(func(e *entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Capture.GetSrt() == actionRevisit {
			a.summary.Revisits++
		}
		f, original, err := a.assess(ctx, e, originals)
		if err != nil {
			return err
		}
		if f.Status == "" {
			return nil
		}
		if !good(f.Status) {
			a.failed = true
		}
		if f.Target != "" {
			var seen bool
			k := "x/missing/" + hashString(f.Target)
			err = a.get(k, &seen)
			if errors.Is(err, badger.ErrKeyNotFound) {
				a.summary.Missing++
				err = a.put(k, true)
			}
			if err != nil {
				return err
			}
		}
		if act != nil {
			if err := act(e, f, original); err != nil {
				return err
			}
		}
		return a.finding(f)
	})
}
func (a *collection) writeReport(w io.Writer) error {
	if _, err := io.WriteString(w, "{\"summary\":"); err != nil {
		return err
	}
	if err := json.MarshalEncode(jsontext.NewEncoder(w), a.summary); err != nil {
		return err
	}
	if _, err := io.WriteString(w, ",\"records\":["); err != nil {
		return err
	}
	first := true
	err := a.scan("x/audit-finding/", func(data []byte) error {
		if !first {
			if _, err := io.WriteString(w, ",\n"); err != nil {
				return err
			}
		}
		first = false
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "]}\n")
	return err
}

type auditOptions struct {
	originals, report, repair string
	backups                   []string
}

func NewCmdAudit() *cobra.Command {
	return newCollectionCommand(false)
}
func NewCmdRecover() *cobra.Command { return newCollectionCommand(true) }

func newCollectionCommand(recoverCollection bool) *cobra.Command {
	var o auditOptions
	destination := "repair-to"
	cmd := &cobra.Command{Use: "audit COLLECTION", Short: "Check revisit targets and payloads, optionally comparing original captures", Args: cobra.ExactArgs(1)}
	if recoverCollection {
		destination = "output-dir"
		cmd.Use = "recover COLLECTION"
		cmd.Short = "Restore missing revisit targets and correct payload digests in deduplicated WARCs"
		cmd.Long = `Recover a deduplicated WARC collection when a revisit points to a missing
capture or its payload digest disagrees with the stored bytes. A revisit refers
to an earlier record (the donor) for its payload. Give COLLECTION as the root
directory of the surviving WARCs, including older files that may contain donors.

Recover scans .warc and .warc.gz files recursively. It can correct legacy
digests that included HTTP chunk framing. Supply --recovery-source to search a
backup for a missing donor, or --originals to replace a faulty revisit with a
verified original capture.

Choose a new --output-dir outside the inputs. Recover copies the surviving
WARCs, applies the repairs, and verifies the staged collection before publishing
it there. If verification finds unresolved revisits, the command exits nonzero
and reports the staging directory. The JSON report goes to stdout by default.`
		cmd.Example = "  warc recover surviving/ --recovery-source backups/ --output-dir recovered/ --report recovery.json"
	}
	cmd.Flags().StringVar(&o.originals, "originals", "", "directory containing original captures")
	cmd.Flags().StringVar(&o.report, "report", "", "new JSON report file (default stdout)")
	help := "stage verified recovery in a fresh directory"
	if !recoverCollection {
		help = "deprecated: use warc recover --output-dir"
	}
	cmd.Flags().StringVar(&o.repair, destination, "", help)
	cmd.Flags().StringArrayVar(&o.backups, "recovery-source", nil, "backup WARC directory (repeatable)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		opts := o
		if !cmd.Flags().Changed(destination) {
			opts.repair = viper.GetString(destination)
		}
		if !cmd.Flags().Changed("originals") {
			opts.originals = viper.GetString("originals")
		}
		if !cmd.Flags().Changed("report") {
			opts.report = viper.GetString("report")
		}
		if !cmd.Flags().Changed("recovery-source") {
			opts.backups = viper.GetStringSlice("recovery-source")
		}
		if recoverCollection && opts.repair == "" {
			return errors.New("recover requires --output-dir")
		}
		if !recoverCollection && opts.repair != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), "deprecated: audit --repair-to; use warc recover --output-dir")
		}
		cmd.SilenceUsage = true
		return opts.run(cmd.Context(), args[0], cmd.OutOrStdout())
	}
	return cmd
}
func openInventory(ctx context.Context, work string, roots ...string) (*collection, error) {
	a, err := newCollection(work)
	if err != nil {
		return nil, err
	}
	for _, root := range roots {
		before := a.summary.Files
		if err := a.inventoryRoot(ctx, root); err != nil {
			return nil, errors.Join(err, a.scratch.Close())
		}
		if a.summary.Files == before {
			a.failed = true
			a.summary.Complete = false
			if err := a.finding(auditFinding{Status: "empty-input", File: root, Detail: "no WARC files found"}); err != nil {
				return nil, errors.Join(err, a.scratch.Close())
			}
		}
	}
	return a, nil
}
func inputRoot(path string) (string, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("expected collection directory: %s", p)
	}
	return p, nil
}
func freshPath(path string, roots []string) (string, error) {
	if path == "" {
		return "", nil
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return "", err
	}
	p = filepath.Join(parent, filepath.Base(p))
	for _, root := range roots {
		rel, err := filepath.Rel(root, p)
		if err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return "", fmt.Errorf("output overlaps input: %s", p)
		}
	}
	if _, err = os.Lstat(p); err == nil {
		return "", fmt.Errorf("destination already exists: %s", p)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return p, nil
}
func (o auditOptions) run(ctx context.Context, path string, out io.Writer) (err error) {
	root, err := inputRoot(path)
	if err != nil {
		return err
	}
	roots := []string{root}
	if o.originals != "" {
		o.originals, err = inputRoot(o.originals)
		if err != nil {
			return err
		}
		roots = append(roots, o.originals)
	}
	for i, p := range o.backups {
		o.backups[i], err = inputRoot(p)
		if err != nil {
			return err
		}
		roots = append(roots, o.backups[i])
	}
	o.repair, err = freshPath(o.repair, roots)
	if err != nil {
		return err
	}
	if o.repair != "" {
		roots = append(roots, o.repair)
	}
	o.report, err = freshPath(o.report, roots)
	if err != nil {
		return err
	}
	if len(o.backups) > 0 && o.repair == "" {
		return errors.New("--recovery-source requires --repair-to")
	}
	work, err := os.MkdirTemp("", "warc-audit-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(work)) }()
	open := func(name string, dirs ...string) (*collection, error) {
		return openInventory(ctx, filepath.Join(work, name), dirs...)
	}
	a, err := open("collection", root)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, a.scratch.Close()) }()
	var originals *collection
	if o.originals != "" {
		originals, err = open("originals", o.originals)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, originals.scratch.Close()) }()
	}
	if originals != nil && !originals.summary.Complete {
		a.summary.Complete = false
		a.failed = true
		if err = a.finding(auditFinding{Status: "unreadable-originals", File: o.originals}); err != nil {
			return err
		}
	}
	if o.repair != "" {
		err = o.recoverCollection(ctx, root, work, a, originals)
	} else {
		err = a.examine(ctx, originals, nil)
	}
	if err != nil {
		return err
	}
	if o.report != "" {
		f, e := os.OpenFile(o.report, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if e != nil {
			return e
		}
		err = errors.Join(a.writeReport(f), f.Sync(), f.Close())
	} else {
		err = a.writeReport(out)
	}
	if a.failed {
		err = errors.Join(err, errors.New("collection audit has unresolved findings or incomplete coverage"))
	}
	return err
}
