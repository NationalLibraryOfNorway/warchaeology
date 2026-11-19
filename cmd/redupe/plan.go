// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package redupe

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"

	"github.com/dgraph-io/badger/v4"
	"github.com/nationallibraryofnorway/gowarc/v3"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/version"
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

type Report struct {
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
			err = errors.Join(err, json.NewEncoder(o.reportWriter).Encode(o.report))
		}
	}()
	if err = o.open(ctx); err != nil {
		return err
	}
	ordered, err := o.inventory(ctx)
	if err != nil {
		return err
	}
	h := sha256.New()
	config, err := json.Marshal(o.configuration)
	if err != nil {
		return err
	}
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
				data, err = json.Marshal(e)
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
	if err = orderedRecords(ordered, func(ref string) error {
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
		if err != nil {
			yr.Failed++
			if e := o.addFinding(o.period(e.Capture), Finding{ref, err.Error(), true}); e != nil {
				return e
			}
			return nil
		}
		return o.put("x/plan/"+hashString(ref), p)
	}); err != nil {
		return err
	}
	if err = o.checkSources(ctx); err != nil {
		return err
	}
	if !o.Audit {
		if err = o.publish(ctx, ordered); err != nil {
			return err
		}
	}
	for _, yr := range o.report.Periods {
		if yr.Failed > 0 {
			return errors.New("blocking findings prevent complete reconstruction; see report")
		}
	}
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
	size := donor.PayloadLength
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
		p.PayloadSHA256, size, err = payloadHash(wr)
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
		key := "x/target/" + p.Period + "/" + hashString(donor.Capture.GetSrt()+":"+identity)
		var target gowarc.RevisitRef
		err = o.get(key, &target)
		if err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
			return nil, err
		}
		if err == nil && referenceSize(&target) < size-o.MinimumSizeGain {
			p.Action, p.Target = actionRevisit, &target
			yr.Revisits++
		} else {
			yr.Full++
			if errors.Is(err, badger.ErrKeyNotFound) {
				target = gowarc.RevisitRef{
					Profile:        gowarc.ProfileIdenticalPayloadDigestV1_1,
					TargetRecordId: c.GetRid(),
					TargetUri:      c.GetUri(),
					TargetDate:     c.GetSts().AsTime().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
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
		yr.Expansion += size
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
	if err := ctx.Err(); err != nil {
		return nil, nil, err
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
	var candidates []*entry
	accept := func(c *cdx.Capture) error {
		candidate, err := o.entry(c.GetRef())
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			candidates = append(candidates, candidate)
		} else if candidates[0].Fingerprint != candidate.Fingerprint {
			return errors.New("ambiguous revisit target")
		}
		return nil
	}
	c := e.Capture
	var err error
	if c.GetRoi() != "" {
		err = o.catalog.CapturesByID(ctx, c.GetRoi(), accept)
	} else {
		if c.GetRou() == "" || c.GetRod() == nil {
			return nil, nil, errors.New("revisit lacks an explicit ID or exact URI and date")
		}
		key := cdx.CanonicalKey(c.GetRou())
		err = o.catalog.Scan(ctx, key+" ", key+"!", func(candidate *cdx.Capture) error {
			if candidate.GetUri() != c.GetRou() || !candidate.GetSts().AsTime().Equal(c.GetRod().AsTime()) {
				return nil
			}
			// Only exclude differing digests when labels use the same algorithm.
			a, _, _ := strings.Cut(c.GetDig(), ":")
			b, _, _ := strings.Cut(candidate.GetDig(), ":")
			if a != "" && strings.EqualFold(a, b) && !digestEqual(c.GetDig(), candidate.GetDig()) {
				return nil
			}
			return accept(candidate)
		})
	}
	if err != nil {
		return nil, nil, err
	}
	if len(candidates) == 0 {
		return nil, nil, errors.New("missing revisit target")
	}
	if len(candidates) != 1 {
		return nil, nil, errors.New("ambiguous revisit target")
	}
	donor, chain, err := o.resolve(ctx, candidates[0], seen)
	if err != nil {
		return nil, nil, err
	}
	if donor.Capture.GetSrt() != "response" && donor.Capture.GetSrt() != "resource" {
		return nil, nil, errors.New("unsupported payload donor type")
	}
	return donor, append([]*entry{candidates[0]}, chain...), nil
}

func (o *RedupeOptions) reconstruct(ctx context.Context, e *entry) (gowarc.WarcRecord, error) {
	donor, chain, err := o.resolve(ctx, e, map[string]bool{})
	if err != nil {
		return nil, err
	}
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

func payloadHash(wr gowarc.WarcRecord) (string, int64, error) {
	r, err := payloadReader(wr)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func digestBytes(label string) (string, []byte, error) {
	algorithm, encoded, ok := strings.Cut(label, ":")
	if !ok {
		return "", nil, fmt.Errorf("invalid digest %q", label)
	}
	algorithm = strings.ToLower(algorithm)
	var size int
	switch algorithm {
	case "md5":
		size = 16
	case "sha1":
		size = 20
	case "sha256":
		size = 32
	case "sha512":
		size = 64
	default:
		return "", nil, fmt.Errorf("unsupported digest algorithm %q", algorithm)
	}
	if len(encoded) == size*2 {
		if b, err := hex.DecodeString(encoded); err == nil {
			return algorithm, b, nil
		}
	}
	if b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(strings.ToUpper(encoded), "=")); err == nil && len(b) == size {
		return algorithm, b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(encoded); err == nil && len(b) == size {
		return algorithm, b, nil
	}
	return "", nil, fmt.Errorf("invalid digest encoding %q", label)
}

func digestEqual(a, b string) bool {
	aa, ab, ae := digestBytes(a)
	ba, bb, be := digestBytes(b)
	return ae == nil && be == nil && aa == ba && string(ab) == string(bb)
}

func verifyDigest(label string, r io.Reader) error {
	if label == "" {
		return nil
	}
	algorithm, expected, err := digestBytes(label)
	if err != nil {
		return err
	}
	var h hash.Hash
	switch algorithm {
	case "md5":
		h = md5.New()
	case "sha1":
		h = sha1.New()
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	}
	if _, err = io.Copy(h, r); err != nil {
		return err
	}
	if string(h.Sum(nil)) != string(expected) {
		return fmt.Errorf("digest mismatch: %s", label)
	}
	return nil
}

func verifyBlock(wr gowarc.WarcRecord) error {
	if err := wr.Block().Cache(); err != nil {
		return err
	}
	r, err := wr.Block().RawBytes()
	if err != nil {
		return err
	}
	return verifyDigest(wr.WarcHeader().Get(gowarc.WarcBlockDigest), r)
}

func verifyRecord(wr gowarc.WarcRecord) error {
	if length, err := wr.ContentLength(); err != nil || length != wr.Block().Size() {
		return errors.New("invalid content length")
	}
	if err := verifyBlock(wr); err != nil {
		return err
	}
	r, err := payloadReader(wr)
	if err != nil {
		return err
	}
	return verifyDigest(wr.WarcHeader().Get(gowarc.WarcPayloadDigest), r)
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
		if err := json.NewEncoder(o.findings).Encode(struct {
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
