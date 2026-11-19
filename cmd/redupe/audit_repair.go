// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package redupe

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dgraph-io/badger/v4"
	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/publication"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcdigest"
)

func validFull(e *entry) bool {
	return e != nil && !e.Truncated && !e.Segmented && e.Issue == "" && e.Audit.Issue == "" && e.Audit.Payload["sha256"] != ""
}

func staleOriginal(e *entry) bool {
	return e != nil && !e.Truncated && !e.Segmented && e.Issue == "" && e.Audit.Issue == "donor payload digest mismatch" && e.Audit.BlockVerified && e.Audit.Payload["sha256"] != ""
}

func (o auditOptions) recoverCollection(ctx context.Context, root, work string, a, originals *collection) (resultErr error) {
	var backups *collection
	if len(o.backups) > 0 {
		var err error
		backups, err = openInventory(ctx, filepath.Join(work, "backups"), o.backups...)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, backups.scratch.Close()) }()
		if !backups.summary.Complete {
			a.summary.Complete = false
			a.failed = true
			if err := a.finding(auditFinding{Status: "unreadable-recovery-source", Detail: "backup scan incomplete"}); err != nil {
				return err
			}
		}
	}
	err := a.examine(ctx, originals, func(e *entry, f auditFinding, original *entry) error {
		return a.planRecovery(ctx, e, f, original, backups)
	})
	if err != nil {
		return err
	}
	for _, src := range []*collection{a, backups} {
		if src != nil {
			if err = a.listCandidates(ctx, src); err != nil {
				return err
			}
		}
	}
	if !a.summary.Complete {
		return nil
	}
	stage, err := os.MkdirTemp(filepath.Dir(o.repair), "."+filepath.Base(o.repair)+".incomplete-")
	if err != nil {
		return err
	}
	a.summary.Destination = stage
	keep := false
	defer func() {
		if !keep {
			resultErr = errors.Join(resultErr, os.RemoveAll(stage))
		}
	}()
	if err = a.checkSources(ctx); err != nil {
		return err
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || !warcPath(path) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return a.stageFile(ctx, path, rel, stage)
	})
	if err != nil {
		return err
	}
	if err = a.restoreDonors(ctx, stage); err != nil {
		return err
	}
	for _, src := range []*collection{a, originals, backups} {
		if src != nil {
			if err = src.checkSources(ctx); err != nil {
				return err
			}
		}
	}
	verified, err := openInventory(ctx, filepath.Join(work, "verification"), stage)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, verified.scratch.Close()) }()
	if err = verified.examine(ctx, originals, nil); err != nil {
		return err
	}
	if err = a.scan("x/expected/", func(data []byte) error {
		var want expectedFull
		if err := json.Unmarshal(data, &want); err != nil {
			return err
		}
		got, err := verified.byID(ctx, want.ID)
		if err != nil {
			return err
		}
		if !validFull(got) || got.Audit.Payload["sha256"] != want.Payload || got.Audit.Header != want.Header {
			verified.failed = true
			return verified.finding(auditFinding{Status: "restored-record-mismatch", ID: want.ID})
		}
		return nil
	}); err != nil {
		return err
	}
	a.summary.Verified = &verified.summary
	a.failed = verified.failed
	if verified.failed {
		if err = verified.scan("x/audit-finding/", func(data []byte) error {
			var f auditFinding
			if err := json.Unmarshal(data, &f); err != nil {
				return err
			}
			if good(f.Status) {
				return nil
			}
			f.Status = "after-recovery-" + f.Status
			return a.finding(f)
		}); err != nil {
			return err
		}
		keep = true
		return nil
	}
	if err = publication.SyncDir(stage); err != nil {
		return err
	}
	if err = publication.Rename(stage, o.repair, false); err != nil {
		return err
	}
	keep = true
	a.summary.Destination = o.repair
	return publication.SyncDir(filepath.Dir(o.repair))
}
func (a *collection) planRecovery(ctx context.Context, e *entry, f auditFinding, original *entry, backups *collection) error {
	if good(f.Status) || e.Capture.GetSrt() != actionRevisit || f.Status == "conflicting-id" {
		return nil
	}
	if validFull(original) || staleOriginal(original) {
		if staleOriginal(original) {
			if err := a.put("x/correct-label/"+hashString(e.Capture.GetRef()), "sha256:"+original.Audit.Payload["sha256"]); err != nil {
				return err
			}
		}
		return a.planReplacement(e, original, original, f.File)
	}
	if backups != nil && (f.Status == "directly-dangling" || f.Status == "transitively-dangling") {
		_, _, err := a.resolve(ctx, e, map[string]bool{})
		var missing *missingTarget
		if !errors.As(err, &missing) {
			return fmt.Errorf("reference changed during recovery planning: %w", err)
		}
		link := missing.link.Capture
		donor, err := backups.lookupTarget(ctx, link)
		if err != nil {
			if err := a.finding(auditFinding{Status: "conflicting-backup", ID: f.ID, File: f.File, Offset: f.Offset, Detail: err.Error()}); err != nil {
				return err
			}
		} else if validFull(donor) && targetMatches(link, donor.Capture) && donor.Audit.matches(missing.link.Audit.Declared) {
			return a.planDonor(ctx, e, donor, f.File)
		}
	}
	alg, value, err := warcdigest.Parse(e.Audit.Declared)
	if err != nil {
		return nil
	}
	return a.put(fmt.Sprintf("x/wanted/%s:%x", alg, value), true)
}

func (a *collection) planReplacement(e, source, headers *entry, file string) error {
	if err := a.expectFull(headers, source.Audit.Payload["sha256"]); err != nil {
		return err
	}
	if err := a.put("x/replace/"+hashString(e.Capture.GetRef()), source.Capture.GetRef()); err != nil {
		return err
	}
	return a.put("x/rewrite/"+hashString(file), true)
}

func (a *collection) planDonor(ctx context.Context, e, donor *entry, file string) error {
	if err := a.put("x/restore/"+hashString(donor.Capture.GetRid()), donor.Capture.GetRef()); err != nil {
		return err
	}
	// Local-CDX pywb filters donors by the literal label. Materialize the
	// capture when the verified backup uses another label for the same bytes.
	if e.Capture.GetDig() == donor.Capture.GetDig() {
		return nil
	}
	_, chain, err := a.resolveWithBackup(ctx, e, map[string]bool{}, donor)
	if err != nil {
		return err
	}
	for _, link := range append([]*entry{e}, chain...) {
		if link.Issue != "" || link.Audit.Issue != "" || (link.Capture.GetSrt() == actionRevisit && !donor.Audit.matches(link.Audit.Declared)) {
			return nil
		}
	}
	if err = a.put("x/materialize/"+hashString(e.Capture.GetRef()), donor); err != nil {
		return err
	}
	return a.planReplacement(e, donor, e, file)
}

func (a *collection) listCandidates(ctx context.Context, source *collection) error {
	// Each matching copy is reported once per digest even if a million
	// revisits reference the same missing donor.
	return source.visitRecords(func(e *entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validFull(e) {
			return nil
		}
		for alg, value := range e.Audit.Payload {
			label := alg + ":" + value
			var wanted bool
			err := a.get("x/wanted/"+label, &wanted)
			if errors.Is(err, badger.ErrKeyNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			f := auditFinding{Status: "digest-only-candidate", ID: e.Capture.GetRid(), Offset: e.Capture.GetOffset(), File: e.Capture.GetFilename(), Digest: label, Detail: e.Capture.GetRef()}
			a.summary.Counts[f.Status]++
			if err := a.put("x/audit-finding/candidate/"+hashString(e.Capture.GetRef()+label), f); err != nil {
				return err
			}
		}
		return nil
	})
}
func writeRecoveryRecord(ctx context.Context, w io.Writer, ref string, compressed bool, label string) (err error) {
	r, err := (loader.FileStorageLoader{}).RecordByStorageRef(ctx, ref)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	if label != "" {
		r.WarcHeader().Set(gowarc.WarcPayloadDigest, label)
	}
	return marshalRecovery(w, r, compressed)
}
func marshalRecovery(w io.Writer, r gowarc.WarcRecord, compressed bool) (err error) {
	if compressed {
		z := gzip.NewWriter(w)
		defer func() { err = errors.Join(err, z.Close()) }()
		w = z
	}
	_, _, err = gowarc.NewMarshaler().Marshal(w, r, 0)
	return err
}
func (a *collection) stageFile(ctx context.Context, path, rel, stage string) (err error) {
	dst := filepath.Join(stage, rel)
	if err = os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, src.Close()) }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Sync(), out.Close(), publication.SyncDir(filepath.Dir(dst))) }()
	var rewrite bool
	err = a.get("x/rewrite/"+hashString(rel), &rewrite)
	if errors.Is(err, badger.ErrKeyNotFound) {
		_, err = io.Copy(out, src)
		return err
	}
	if err != nil {
		return err
	}
	var cursor int64
	err = a.scan("x/offset/"+hashString(rel)+"/", func(data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var ref string
		if err := json.Unmarshal(data, &ref); err != nil {
			return err
		}
		e, err := a.entry(ref)
		if err != nil {
			return err
		}
		c := e.Capture
		if c.GetOffset() < cursor || c.GetRle() <= 0 {
			return errors.New("invalid record extent")
		}
		if _, err := io.CopyN(out, src, c.GetOffset()-cursor); err != nil {
			return err
		}
		offset, err := out.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		var replacement string
		err = a.get("x/replace/"+hashString(ref), &replacement)
		action := "copied-record"
		switch {
		case errors.Is(err, badger.ErrKeyNotFound):
			_, err = io.CopyN(out, src, c.GetRle())
		case err == nil:
			compressed := strings.HasSuffix(strings.ToLower(rel), ".gz")
			var backup entry
			err = a.get("x/materialize/"+hashString(ref), &backup)
			if errors.Is(err, badger.ErrKeyNotFound) {
				action = "restored-original"
				var label string
				err = a.get("x/correct-label/"+hashString(ref), &label)
				if err == nil || errors.Is(err, badger.ErrKeyNotFound) {
					err = writeRecoveryRecord(ctx, out, replacement, compressed, label)
				}
				if label != "" {
					action = "restored-original-corrected-digest"
				}
			} else if err == nil {
				action = "restored-from-donor"
				err = a.materialize(ctx, e, &backup, out, compressed)
			}
			if err == nil {
				_, err = src.Seek(c.GetRle(), io.SeekCurrent)
			}
		}
		if err != nil {
			return err
		}
		cursor = c.GetOffset() + c.GetRle()
		return a.finding(auditFinding{Status: action, File: rel, Offset: c.GetOffset(), NewOffset: &offset, ID: c.GetRid(), Detail: replacement})
	})
	if err != nil {
		return err
	}
	_, err = io.Copy(out, src)
	return err
}
func (a *collection) restoreDonors(ctx context.Context, stage string) (err error) {
	var out *os.File
	defer func() {
		if out != nil {
			err = errors.Join(err, out.Sync(), out.Close())
		}
	}()
	return a.scan("x/restore/", func(data []byte) error {
		if out == nil {
			var e error
			out, e = os.CreateTemp(stage, "recovered-donors-*.warc")
			if e != nil {
				return e
			}
		}
		var ref string
		if err := json.Unmarshal(data, &ref); err != nil {
			return err
		}
		offset, err := out.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if err := writeRecoveryRecord(ctx, out, ref, false, ""); err != nil {
			return err
		}
		return a.finding(auditFinding{Status: "restored-donor", File: filepath.Base(out.Name()), Offset: offset, Detail: ref})
	})
}

type expectedFull struct{ ID, Header, Payload string }

func (a *collection) expectFull(e *entry, payload string) error {
	return a.put("x/expected/"+hashString(e.Capture.GetRid()), expectedFull{e.Capture.GetRid(), e.Audit.Header, payload})
}
func (a *collection) materialize(ctx context.Context, e, backup *entry, w io.Writer, compressed bool) (err error) {
	donor, chain, err := a.resolveWithBackup(ctx, e, map[string]bool{}, backup)
	if err != nil {
		return err
	}
	r, err := rebuildChain(ctx, e, donor, chain)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	return marshalRecovery(w, r, compressed)
}
