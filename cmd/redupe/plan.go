// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package redupe

import (
	"bufio"
	"context"
	"crypto/sha256"
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
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/publication"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/version"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warc"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcdigest"
)

const (
	actionRevisit = "revisit"
	actionFull    = "full"
)

type Finding struct {
	Source   string `json:"source"`
	Message  string `json:"message"`
	Blocking bool   `json:"blocking"`
}

type PeriodReport struct {
	Selected    int64     `json:"selectedRecords"`
	Aliases     int64     `json:"aliases"`
	Referenced  int64     `json:"referencedSourceRecords"`
	CrossPeriod int64     `json:"crossPeriodPayloadDependencies"`
	Full        int64     `json:"plannedFullRecords"`
	Revisits    int64     `json:"plannedRevisits"`
	Expansion   int64     `json:"estimatedUncompressedPayloadExpansionBytes"`
	Verified    int64     `json:"verified"`
	Unchecked   int64     `json:"unchecked"`
	Failed      int64     `json:"failed"`
	Findings    []Finding `json:"findings,omitempty"`
	Completed   bool      `json:"completed"`
}

type previewState struct {
	OrderedSHA256      string
	PlanReportSHA256   string
	PlanFindingsSHA256 string
	Kind               string
	Settings           string
	Fingerprint        string
}
type Report struct {
	Reused           []string                 `json:"reusedPhases,omitempty"`
	RetainedType     string                   `json:"retainedState,omitempty"`
	RetainedPath     string                   `json:"retainedPath,omitempty"`
	RetainedBytes    int64                    `json:"retainedBytes,omitzero"`
	FindingsArtifact string                   `json:"findingsArtifact,omitempty"`
	FindingCount     int64                    `json:"findingCount"`
	Version          string                   `json:"toolVersion"`
	Configuration    map[string]any           `json:"configuration"`
	Fingerprint      string                   `json:"inventoryConfigurationSha256"`
	Audit            bool                     `json:"audit"`
	Planning         string                   `json:"planning"`
	Periods          map[string]*PeriodReport `json:"periods"`
	Findings         []Finding                `json:"findings,omitempty"`
}

type planned struct {
	Ref           string             `json:"sourceRef"`
	ID            string             `json:"recordId"`
	Period        string             `json:"period"`
	Action        string             `json:"action"`
	Donor         string             `json:"payloadDonor,omitempty"`
	Target        *gowarc.RevisitRef `json:"target,omitempty"`
	PayloadSHA256 string             `json:"payloadSha256,omitempty"`
}

func (o *RedupeOptions) period(c *cdx.Capture) string {
	layout := "2006"
	switch o.Interval {
	case "month":
		layout = "2006-01"
	case "day":
		layout = "2006-01-02"
	}
	return c.GetSts().AsTime().UTC().Format(layout)
}

func (o *RedupeOptions) periodReport(y string) *PeriodReport {
	if o.report.Periods[y] == nil {
		o.report.Periods[y] = &PeriodReport{}
	}
	return o.report.Periods[y]
}

func orderedRecords(path string, visit func(string) error) (err error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		fields := strings.Fields(line)
		if len(fields) != 4 {
			return errors.New("invalid sorted inventory")
		}
		ref, err := hex.DecodeString(fields[3])
		if err != nil {
			return err
		}
		if err = visit(string(ref)); err != nil {
			return err
		}
	}
}

func (o *RedupeOptions) Run(ctx context.Context) (err error) {
	if err = o.Validate(); err != nil {
		return err
	}
	if err = o.checkReportPaths(ctx); err != nil {
		return err
	}
	if o.configuration == nil {
		o.configuration = map[string]any{}
	}
	o.configuration["interval"] = o.Interval
	o.report = Report{Version: version.SoftwareVersion(), Configuration: o.configuration, Audit: o.Audit,
		Planning: "verified payload identities", Periods: map[string]*PeriodReport{}}
	if o.Audit && !o.VerifyPayloads {
		o.report.Planning = "metadata estimates; payload identities and size savings unchecked"
	}
	defer func() {
		if err != nil {
			if e := o.addFinding("", Finding{Message: err.Error(), Blocking: true}); e != nil {
				err = errors.Join(err, e)
			}
		}
		if o.ReportPath != "" {
			err = errors.Join(err, writeJSON(o.ReportPath, o.report))
		} else if o.reportWriter != nil {
			err = errors.Join(err, json.MarshalEncode(jsontext.NewEncoder(o.reportWriter), o.report))
		}
		if err == nil && o.Audit && !o.temporaryIndex {
			if err = ctx.Err(); err == nil {
				err = o.put("!preview", o.preview)
			}
			o.keepInventory = err == nil
		}
	}()
	if err = o.open(ctx); err != nil {
		return err
	}
	config, err := json.Marshal(o.configuration, json.Deterministic(true))
	if err != nil {
		return err
	}
	settings := hashString(string(config))
	ordered := filepath.Join(o.workDir, "ordered")
	if o.keepInventory {
		sum, err := fileChecksum(ordered)
		if err != nil {
			return fmt.Errorf("run a fresh preview: %w", err)
		}
		if sum != o.preview.OrderedSHA256 {
			return errors.New("sorted inventory checksum mismatch; run a fresh preview")
		}
		if settings != o.preview.Settings {
			return errors.New("effective settings changed; run a fresh preview")
		}
		if err = o.checkSelection(ctx); err != nil {
			return fmt.Errorf("run a fresh preview: %w", err)
		}
		o.report.Fingerprint = o.preview.Fingerprint
		o.report.Reused = []string{"inventory", "sort"}
	} else {
		if _, err = o.inventory(ctx); err != nil {
			return err
		}
		if err = o.fingerprint(config); err != nil {
			return err
		}
	}
	if o.preview.Kind == "verified-plan" {
		if err = o.restorePlanReport(); err != nil {
			return fmt.Errorf("run a fresh preview: %w", err)
		}
		o.report.Reused = append(o.report.Reused, "verified planning")
	} else {
		if o.keepInventory {
			if err = o.clearPlan(); err != nil {
				return err
			}
		}
		if err = o.planOrdered(ctx, ordered); err != nil {
			return err
		}
	}
	if err = o.checkSources(ctx); err != nil {
		return err
	}
	if !o.Audit {
		if err = o.publish(ctx); err != nil {
			return err
		}
	}
	for _, yr := range o.report.Periods {
		if yr.Failed > 0 {
			return errors.New("blocking findings prevent complete reconstruction; see report")
		}
	}
	if o.Audit && !o.temporaryIndex {
		return o.retainPreview(ctx, settings)
	}
	return nil
}

func (o *RedupeOptions) retainPreview(ctx context.Context, settings string) error {
	var err error
	if err = o.checkSelection(ctx); err != nil {
		return err
	}
	o.report.RetainedType = "inventory"
	if o.VerifyPayloads {
		o.report.RetainedType = "verified-plan"
		if err = o.savePlanReport(); err != nil {
			return err
		}
	} else if err = o.clearPlan(); err != nil {
		return err
	}
	err = errors.Join(o.findings.Sync(), o.findings.Close(), o.FileWalker.Close(), o.scratch.Close())
	o.findings, o.scratch = nil, nil
	if err != nil {
		return err
	}
	o.report.RetainedPath = o.workDir
	err = filepath.WalkDir(o.workDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err == nil {
			o.report.RetainedBytes += info.Size()
		}
		return err
	})
	if err != nil {
		return err
	}
	if err = publication.SyncDir(o.workDir); err != nil {
		return err
	}
	if err = publication.SyncDir(o.IndexDir); err != nil {
		return err
	}
	o.preview.Kind = o.report.RetainedType
	o.preview.Settings = settings
	o.preview.Fingerprint = o.report.Fingerprint
	return nil
}

func (o *RedupeOptions) planOrdered(ctx context.Context, ordered string) error {
	return orderedRecords(ordered, func(ref string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, err := o.entry(ref)
		if err != nil {
			return err
		}
		yr := o.periodReport(o.period(e.Capture))
		yr.Selected++
		p, err := o.plan(ctx, e)
		if diskspace.Exhausted(err) {
			return fmt.Errorf("%s: %w", ref, err)
		}
		if err != nil {
			yr.Failed++
			if e := o.addFinding(o.period(e.Capture), Finding{ref, err.Error(), true}); e != nil {
				return e
			}
			return nil
		}
		// Selected is the ordinal within this period in the sorted inventory.
		return o.put(fmt.Sprintf("x/plan/%s/%020d", p.Period, yr.Selected), p)
	})
}

func (o *RedupeOptions) savePlanReport() error {
	path := filepath.Join(o.workDir, "plan-report.json")
	if err := writeJSON(path, o.report); err != nil {
		return err
	}
	sum, err := fileChecksum(path)
	if err != nil {
		return err
	}
	o.preview.PlanReportSHA256 = sum
	if err := o.findings.Sync(); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(o.workDir, "plan-findings.jsonl"))
	if err != nil {
		return err
	}
	o.preview.PlanFindingsSHA256, err = copyFindings(f, o.report.FindingsArtifact)
	return errors.Join(err, f.Sync(), f.Close())
}

func (o *RedupeOptions) restorePlanReport() error {
	var saved Report
	data, err := os.ReadFile(filepath.Join(o.workDir, "plan-report.json"))
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != o.preview.PlanReportSHA256 {
		return errors.New("plan report checksum mismatch")
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return err
	}
	sum, err := copyFindings(o.findings, filepath.Join(o.workDir, "plan-findings.jsonl"))
	if err == nil && sum != o.preview.PlanFindingsSHA256 {
		err = errors.New("plan findings checksum mismatch")
	}
	if err != nil {
		// Discard the unverified copy before Run records the failure.
		_, seekErr := o.findings.Seek(0, io.SeekStart)
		return errors.Join(err, seekErr, o.findings.Truncate(0))
	}
	o.report.Findings, o.report.FindingCount, o.report.Periods = saved.Findings, saved.FindingCount, saved.Periods
	return nil
}

func copyFindings(dst io.Writer, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(dst, h), f)
	return hex.EncodeToString(h.Sum(nil)), errors.Join(err, f.Close())
}

func (o *RedupeOptions) clearPlan() error {
	return o.scratch.DropPrefix([]byte("x/plan/"), []byte("x/alias/"), []byte("x/referenced/"), []byte("x/target/"))
}

func (o *RedupeOptions) fingerprint(config []byte) error {
	h := sha256.New()
	var err error
	_, _ = h.Write(config)
	for _, prefix := range []string{"x/source/", "x/record/"} {
		if err = o.scan(prefix, func(data []byte) error {
			if prefix == "x/record/" {
				var e entry
				if err := decodeEntry(data, &e); err != nil {
					return err
				}
				e.Capture.Generation = ""
				var err error
				data, err = json.Marshal(e, json.Deterministic(true))
				if err != nil {
					return err
				}
			}
			_, err := h.Write(data)
			return err
		}); err != nil {
			return err
		}
	}
	o.report.Fingerprint = hex.EncodeToString(h.Sum(nil))
	return nil
}

func (o *RedupeOptions) plan(ctx context.Context, e *entry) (_ *planned, resultErr error) {
	c := e.Capture
	yr := o.periodReport(o.period(c))
	p := &planned{Ref: c.GetRef(), ID: c.GetRid(), Period: o.period(c), Action: "copy"}
	if e.Issue != "" {
		return nil, errors.New(e.Issue)
	}
	if e.Segmented {
		return nil, errors.New("unsupported segmented record")
	}
	if err := o.catalog.CapturesByID(ctx, c.GetRid(), func(other *cdx.Capture) error {
		candidate, err := o.entry(other.GetRef())
		if err != nil {
			return err
		}
		if e.Fingerprint != candidate.Fingerprint {
			return fmt.Errorf("conflicting record ID %s", c.GetRid())
		}
		return nil
	}); err != nil {
		return nil, err
	}
	var alias string
	aliasKey := "x/alias/" + hashString(c.GetRid())
	if err := o.get(aliasKey, &alias); err == nil {
		p.Action, p.Donor = "alias", alias
		yr.Aliases++
		return p, nil
	} else if !errors.Is(err, badger.ErrKeyNotFound) {
		return nil, err
	}
	donor, chain, err := o.resolve(ctx, e, map[string]bool{})
	if err != nil {
		return nil, err
	}
	if err := o.reportDependencies(p.Period, chain); err != nil {
		return nil, err
	}

	if c.GetSrt() == actionRevisit {
		p.Donor = donor.Capture.GetRef()
	}
	if err := o.reportContext(ctx, e, p.Period); err != nil {
		return nil, err
	}

	identity := donor.Capture.GetDig()
	size := donor.StoredBodyLength
	var wr gowarc.WarcRecord
	if !o.Audit || o.VerifyPayloads {
		wr, err = o.reconstruct(ctx, e)
		if err != nil {
			return nil, err
		}
		defer func() { resultErr = errors.Join(resultErr, wr.Close()) }()
		if err = verifyRecord(wr); err != nil {
			return nil, err
		}
		size = warc.StoredBodyLength(wr)
		p.PayloadSHA256, err = payloadHash(wr)
		if err != nil {
			return nil, err
		}
		identity = p.PayloadSHA256
		yr.Verified++
	} else {
		yr.Unchecked++
		if identity == "" {
			identity = "source:" + donor.Capture.GetRef()
		}
	}
	eligible := donor.Capture.GetSrt() == "response" || donor.Capture.GetSrt() == "resource"
	if eligible {
		p.Action = actionFull
	}
	if eligible && !donor.Truncated {
		key := "x/target/" + p.Period + "/" + hashString(donor.Capture.GetSrt()+":"+identity)
		var target gowarc.RevisitRef
		err = o.get(key, &target)
		if err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
			return nil, err
		}
		if err == nil && !e.HasTrailers && referenceSize(&target) < size-o.MinimumSizeGain {
			p.Action, p.Target = actionRevisit, &target
			yr.Revisits++
		} else {
			yr.Full++
			if errors.Is(err, badger.ErrKeyNotFound) {
				target = gowarc.RevisitRef{
					Profile:        gowarc.ProfileIdenticalPayloadDigestV1_1,
					TargetRecordId: c.GetRid(),
					TargetUri:      c.GetUri(),
					TargetDate:     timestamp.FormatRFC3339(c.GetSts().AsTime()),
				}
				if err = o.put(key, target); err != nil {
					return nil, err
				}
			}
		}
	} else {
		yr.Full++
	}
	if c.GetSrt() == actionRevisit && p.Action == actionFull {
		yr.Expansion += donor.Capture.GetPle()
	}
	if err = o.put(aliasKey, p.Ref); err != nil {
		return nil, err
	}
	return p, nil
}

func referenceSize(r *gowarc.RevisitRef) int64 {
	return int64(len(gowarc.WarcRefersTo) + len(r.TargetRecordId) + len(gowarc.WarcProfile) + len(r.Profile) + len(gowarc.WarcRefersToTargetURI) + len(r.TargetUri) + len(gowarc.WarcRefersToDate) + len(r.TargetDate))
}

func (o *RedupeOptions) resolve(ctx context.Context, e *entry, seen map[string]bool) (*entry, []*entry, error) {
	return o.resolveWithBackup(ctx, e, seen, nil)
}

func (o *RedupeOptions) resolveWithBackup(ctx context.Context, e *entry, seen map[string]bool, backup *entry) (*entry, []*entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if e.Issue == invalidRevisitDate {
		return nil, nil, errors.New(e.Issue)
	}
	ref := e.Capture.GetRef()
	if seen[ref] {
		return nil, nil, errors.New("revisit reference cycle")
	}
	if len(seen) >= 32 {
		return nil, nil, errors.New("revisit chain exceeds 32 records")
	}
	if e.Segmented {
		return nil, nil, errors.New("unsupported segmented payload dependency")
	}
	if e.Capture.GetSrt() != actionRevisit {
		return e, nil, nil
	}
	if e.Profile != gowarc.ProfileIdenticalPayloadDigestV1_0 && e.Profile != gowarc.ProfileIdenticalPayloadDigestV1_1 {
		return nil, nil, fmt.Errorf("unsupported revisit profile %q", e.Profile)
	}
	seen[ref] = true
	target, err := o.lookupTarget(ctx, e.Capture)
	if err != nil {
		return nil, nil, err
	}
	if target == nil && backup != nil && targetMatches(e.Capture, backup.Capture) {
		target = backup
	}
	if target == nil {
		return nil, nil, &missingTarget{link: e}
	}
	if !targetMatches(e.Capture, target.Capture) {
		return nil, nil, errors.New("conflicting target URI or date")
	}
	if target.Truncated {
		return nil, nil, errors.New("truncated donor")
	}
	donor, chain, err := o.resolveWithBackup(ctx, target, seen, backup)
	if err != nil {
		return nil, nil, err
	}
	if donor.Capture.GetSrt() != "response" && donor.Capture.GetSrt() != "resource" {
		return nil, nil, errors.New("unsupported payload donor type")
	}
	return donor, append([]*entry{target}, chain...), nil
}

func targetMatches(link, candidate *cdx.Capture) bool {
	return (link.GetRoi() != "" || link.GetRou() != "" && link.GetRod() != nil) &&
		(link.GetRoi() == "" || link.GetRoi() == candidate.GetRid()) &&
		(link.GetRou() == "" || link.GetRou() == candidate.GetUri()) &&
		(link.GetRod() == nil || link.GetRod().AsTime().Equal(candidate.GetSts().AsTime()))
}
func (o *RedupeOptions) lookupTarget(ctx context.Context, c *cdx.Capture) (*entry, error) {
	var found *entry
	accept := func(c *cdx.Capture) error {
		candidate, err := o.entry(c.GetRef())
		if err != nil {
			return err
		}
		if found == nil {
			found = candidate
		} else if found.Fingerprint != candidate.Fingerprint {
			return errors.New("ambiguous revisit target")
		}
		return nil
	}
	var err error
	if c.GetRoi() != "" {
		err = o.catalog.CapturesByID(ctx, c.GetRoi(), accept)
	} else {
		if c.GetRou() == "" || c.GetRod() == nil {
			return nil, errors.New("revisit lacks an explicit ID or exact URI and date")
		}
		key := cdx.CanonicalKey(c.GetRou())
		err = o.catalog.Scan(ctx, key+" ", key+"!", func(candidate *cdx.Capture) error {
			if candidate.GetUri() != c.GetRou() || !candidate.GetSts().AsTime().Equal(c.GetRod().AsTime()) {
				return nil
			}
			if !o.collectionAudit {
				// Only exclude differing digests when labels use the same algorithm.
				a, _, _ := strings.Cut(c.GetDig(), ":")
				b, _, _ := strings.Cut(candidate.GetDig(), ":")
				if a != "" && strings.EqualFold(a, b) && !warcdigest.Equal(c.GetDig(), candidate.GetDig()) {
					return nil
				}
			}
			return accept(candidate)
		})
	}
	if err != nil {
		return nil, err
	}
	return found, nil
}

func (o *RedupeOptions) reconstruct(ctx context.Context, e *entry) (gowarc.WarcRecord, error) {
	donor, chain, err := o.resolve(ctx, e, map[string]bool{})
	if err != nil {
		return nil, err
	}
	return rebuildChain(ctx, e, donor, chain)
}

func rebuildChain(ctx context.Context, e, donor *entry, chain []*entry) (gowarc.WarcRecord, error) {
	l := loader.FileStorageLoader{}
	wr, err := l.RecordByStorageRef(ctx, donor.Capture.GetRef())
	if err != nil {
		return nil, err
	}
	if err = verifyRecord(wr); err != nil {
		_ = wr.Close()
		return nil, err
	}
	// Validate every link's payload digest, including intermediate revisits.
	links := append([]*entry{e}, chain...)
	for i := len(links) - 2; i >= 0; i-- {
		capture, err := l.RecordByStorageRef(ctx, links[i].Capture.GetRef())
		if err != nil {
			_ = wr.Close()
			return nil, err
		}
		if err = verifyBlock(capture); err != nil {
			return nil, errors.Join(err, capture.Close(), wr.Close())
		}
		if err = verifyPayload(wr, capture.WarcHeader().Get(gowarc.WarcPayloadDigest)); err != nil {
			return nil, errors.Join(err, capture.Close(), wr.Close())
		}
		rebuilt, buildErr := loader.ReconstructIdentical(capture, wr)
		closeErr := errors.Join(capture.Close(), wr.Close())
		if buildErr != nil || closeErr != nil {
			if rebuilt != nil {
				_ = rebuilt.Close()
			}
			return nil, errors.Join(buildErr, closeErr)
		}
		wr = rebuilt
		if err = verifyRecord(wr); err != nil {
			return nil, errors.Join(err, wr.Close())
		}
	}
	return wr, nil
}

func payloadReader(wr gowarc.WarcRecord) (io.Reader, error) {
	if block, ok := wr.Block().(gowarc.PayloadBlock); ok {
		return block.PayloadBytes()
	}
	return wr.Block().RawBytes()
}

func payloadHash(wr gowarc.WarcRecord) (string, error) {
	r, err := payloadReader(wr)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), err
}

func verifyBlock(wr gowarc.WarcRecord) error {
	if err := wr.Block().Cache(); err != nil {
		return err
	}
	r, err := wr.Block().RawBytes()
	if err != nil {
		return err
	}
	return warcdigest.Verify(wr.WarcHeader().Get(gowarc.WarcBlockDigest), r)
}

func verifyRecord(wr gowarc.WarcRecord) error {
	if length, err := wr.ContentLength(); err != nil || length != wr.Block().Size() {
		return errors.New("invalid content length")
	}
	if err := verifyBlock(wr); err != nil {
		return err
	}
	return verifyPayload(wr, wr.WarcHeader().Get(gowarc.WarcPayloadDigest))
}

func verifyPayload(wr gowarc.WarcRecord, label string) error {
	r, err := payloadReader(wr)
	if err != nil {
		return err
	}
	return warcdigest.Verify(label, r)
}

func (o *RedupeOptions) reportContext(ctx context.Context, e *entry, period string) error {
	for _, id := range e.Context {
		found := false
		samePeriod := false
		if err := o.catalog.CapturesByID(ctx, id, func(other *cdx.Capture) error {
			found = true
			samePeriod = samePeriod || o.period(other) == period
			return nil
		}); err != nil {
			return err
		}
		if !found || !samePeriod {
			if err := o.addFinding(period, Finding{e.Capture.GetRef(), "contextual reference outside this period: " + id, false}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *RedupeOptions) reportDependencies(y string, chain []*entry) error {
	yr := o.periodReport(y)
	for _, dependency := range chain {
		key := "x/referenced/" + y + "/" + hashString(dependency.Capture.GetRef())
		var found bool
		err := o.get(key, &found)
		if err == nil {
			continue
		}
		if !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}
		if err = o.put(key, true); err != nil {
			return err
		}
		yr.Referenced++
		if o.period(dependency.Capture) != y {
			yr.CrossPeriod++
		}
	}
	return nil
}

func (o *RedupeOptions) addFinding(y string, f Finding) error {
	o.report.FindingCount++
	if o.findings != nil {
		if err := json.MarshalEncode(jsontext.NewEncoder(o.findings), struct {
			Finding

			Period string `json:"period,omitempty"`
		}{f, y}); err != nil {
			return err
		}
	}
	if y == "" {
		if len(o.report.Findings) < 100 {
			o.report.Findings = append(o.report.Findings, f)
		}
	} else {
		r := o.periodReport(y)
		if len(r.Findings) < 100 {
			r.Findings = append(r.Findings, f)
		}
	}
	return nil
}
