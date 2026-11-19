// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package redupe

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcwriterconfig"
	"github.com/spf13/afero"
)

var testPayload = strings.Repeat("archival payload\n", 400)

func full(t *testing.T, id, date string) gowarc.WarcRecord {
	t.Helper()
	return fullWithPayload(t, id, date, testPayload)
}

func fullWithPayload(t *testing.T, id, date, payload string) gowarc.WarcRecord {
	t.Helper()
	b := gowarc.NewRecordBuilder(gowarc.Response, gowarc.WithAddMissingDigest(true))
	b.AddWarcHeader(gowarc.WarcRecordID, "<urn:uuid:"+id+">")
	b.AddWarcHeader(gowarc.WarcDate, date)
	b.AddWarcHeader(gowarc.WarcTargetURI, "https://example.org/"+id)
	b.AddWarcHeader(gowarc.ContentType, "application/http; msgtype=response")
	if _, err := fmt.Fprintf(b, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nX-Capture: %s\r\nContent-Length: %d\r\n\r\n%s", id, len(payload), payload); err != nil {
		t.Fatal(err)
	}
	wr, _, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wr.Close() })
	return wr
}

func revisit(t *testing.T, id, date string, target gowarc.WarcRecord) gowarc.WarcRecord {
	t.Helper()
	original := full(t, id, date)
	ref := &gowarc.RevisitRef{
		Profile:        gowarc.ProfileIdenticalPayloadDigestV1_1,
		TargetRecordId: target.RecordId(),
		TargetUri:      target.WarcHeader().Get(gowarc.WarcTargetURI),
		TargetDate:     target.WarcHeader().Get(gowarc.WarcDate),
	}
	wr, err := original.ToRevisitRecord(ref)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wr.Close() })
	return wr
}

func fixtureFile(t *testing.T, records ...gowarc.WarcRecord) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.warc")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	m := gowarc.NewMarshaler()
	for _, wr := range records {
		if _, _, err := m.Marshal(f, wr, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func testOptions(t *testing.T, paths ...string) *RedupeOptions {
	t.Helper()
	out := t.TempDir()
	config, err := warcwriterconfig.New("redupe", warcwriterconfig.WithOutDir(out), warcwriterconfig.WithCompress(true), warcwriterconfig.WithConcurrentWriters(1))
	if err != nil {
		t.Fatal(err)
	}
	o := &RedupeOptions{
		Paths:           paths,
		OutputDir:       out,
		IndexDir:        t.TempDir(),
		Concurrency:     2,
		MinimumSizeGain: 0,
		FileWalker:      filewalker.New(filewalker.WithFs(afero.NewOsFs())),
		WriterConfig:    config,
		configuration:   map[string]any{"policy": "test"},
	}
	t.Cleanup(func() {
		if err := o.Close(); err != nil {
			t.Error(err)
		}
	})
	return o
}

func readManifest(t *testing.T, dir, y string) []manifestEntry {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, y, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, f)
	var entries []manifestEntry
	decoder := json.NewDecoder(f)
	for {
		var e manifestEntry
		err := decoder.Decode(&e)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	return entries
}

func TestThreeYearsIndependentAndDeterministic(t *testing.T) {
	a := full(t, "a", "2022-12-31T23:59:59Z")
	b := revisit(t, "b", "2023-01-01T00:00:00Z", a)
	c := revisit(t, "c", "2023-06-01T00:00:00Z", b)
	d := revisit(t, "d", "2024-01-01T00:00:00Z", c)
	e := full(t, "e", "2024-06-01T00:00:00Z")
	source1 := fixtureFile(t, c, a, e)
	source2 := fixtureFile(t, d, b)
	var previous []planned
	for _, concurrency := range []int{1, 4} {
		paths := []string{source1, source2}
		if concurrency == 4 {
			paths[0], paths[1] = paths[1], paths[0]
		}
		o := testOptions(t, paths...)
		o.Concurrency = concurrency
		if err := o.Run(t.Context()); err != nil {
			t.Fatalf("run: %v; report: %+v", err, o.report)
		}
		var plans []planned
		for _, y := range []string{"2022", "2023", "2024"} {
			if !o.report.Periods[y].Completed {
				t.Fatal("year not complete", y)
			}
			manifest := readManifest(t, o.OutputDir, y)
			for i, m := range manifest {
				plans = append(plans, m.planned)
				if i == 0 && m.Action != "full" {
					t.Fatal("year did not start with a full capture", m)
				}
				if i > 0 && (m.Action != "revisit" || m.Target.TargetRecordId != manifest[0].ID) {
					t.Fatal("not a direct in-year revisit", m)
				}
			}
			// Copy only this year to a separate location and verify using its own files.
			isolated := filepath.Join(t.TempDir(), y)
			if err := os.CopyFS(isolated, os.DirFS(filepath.Join(o.OutputDir, y))); err != nil {
				t.Fatal(err)
			}
			if err := o.verifyPeriod(t.Context(), isolated, y); err != nil {
				t.Fatal(err)
			}
			// Capture-specific protocol headers must survive reconstruction.
			for _, m := range manifest {
				if m.Action != "full" {
					continue
				}
				f, err := os.Open(filepath.Join(isolated, m.Output.File))
				if err != nil {
					t.Fatal(err)
				}
				r, err := gowarc.NewWarcFileReaderFromStream(f, m.Output.Offset)
				if err != nil {
					t.Fatal(err)
				}
				rec, err := r.Next()
				if err != nil {
					t.Fatal(err)
				}
				id := strings.TrimPrefix(m.ID, "urn:uuid:")
				if !bytes.Contains(rec.WarcRecord.Block().(gowarc.ProtocolHeaderBlock).ProtocolHeaderBytes(), []byte("X-Capture: "+id)) {
					t.Fatal("lost capture headers")
				}
				_ = rec.Close()
				_ = r.Close()
			}
		}
		if previous != nil && !reflect.DeepEqual(plans, previous) {
			t.Fatal("selection changed with input order/concurrency")
		}
		previous = plans
	}
}

func TestBlockingFindings(t *testing.T) {
	cases := []struct {
		name    string
		records func(*testing.T) []gowarc.WarcRecord
		message string
	}{
		{"missing", func(t *testing.T) []gowarc.WarcRecord {
			t.Helper()
			a := full(t, "a", "2022-01-01T00:00:00Z")
			return []gowarc.WarcRecord{revisit(t, "b", "2023-01-01T00:00:00Z", a)}
		}, "missing revisit target"},
		{"conflict", func(t *testing.T) []gowarc.WarcRecord {
			t.Helper()
			return []gowarc.WarcRecord{full(t, "a", "2022-01-01T00:00:00Z"), full(t, "a", "2022-02-01T00:00:00Z")}
		}, "conflicting record ID"},
		{"cycle", func(t *testing.T) []gowarc.WarcRecord {
			t.Helper()
			a := full(t, "a", "2022-01-01T00:00:00Z")
			b := revisit(t, "b", "2023-01-01T00:00:00Z", a)
			b.WarcHeader().SetId(gowarc.WarcRefersTo, b.RecordId())
			return []gowarc.WarcRecord{b}
		}, "cycle"},
		{"profile", func(t *testing.T) []gowarc.WarcRecord {
			t.Helper()
			a := full(t, "a", "2022-01-01T00:00:00Z")
			b := revisit(t, "b", "2023-01-01T00:00:00Z", a)
			b.WarcHeader().Set(gowarc.WarcProfile, gowarc.ProfileServerNotModifiedV1_1)
			return []gowarc.WarcRecord{a, b}
		}, "unsupported revisit profile"},
		{"segment", func(t *testing.T) []gowarc.WarcRecord {
			t.Helper()
			a := full(t, "a", "2022-01-01T00:00:00Z")
			a.WarcHeader().Set(gowarc.WarcSegmentNumber, "1")
			return []gowarc.WarcRecord{a}
		}, "segmented"},
		{"digest", func(t *testing.T) []gowarc.WarcRecord {
			t.Helper()
			a := full(t, "a", "2022-01-01T00:00:00Z")
			a.WarcHeader().Set(gowarc.WarcPayloadDigest, "sha256:"+strings.Repeat("0", 64))
			return []gowarc.WarcRecord{a}
		}, "digest mismatch"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			o := testOptions(t, fixtureFile(t, tt.records(t)...))
			o.Audit = true
			o.VerifyPayloads = true
			if err := o.Run(t.Context()); err == nil {
				t.Fatal("expected failure")
			}
			data, _ := json.Marshal(o.report)
			if !strings.Contains(string(data), tt.message) {
				t.Fatalf("missing finding %s: %s", tt.message, data)
			}
			files, _ := os.ReadDir(o.OutputDir)
			if len(files) != 0 {
				t.Fatal("audit wrote output")
			}
		})
	}
}

func TestAliasesAndRestart(t *testing.T) {
	a := full(t, "a", "2023-01-01T00:00:00Z")
	path := fixtureFile(t, a, a)
	t.Chdir(filepath.Dir(path))
	o := testOptions(t, path, filepath.Base(path))
	if err := o.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if o.report.Periods["2023"].Aliases != 1 {
		t.Fatal("duplicate was not an alias")
	}
	manifest := readManifest(t, o.OutputDir, "2023")
	if len(manifest) != 2 || manifest[0].Output != manifest[1].Output {
		t.Fatal("alias provenance missing", manifest)
	}
	fingerprint := o.report.Fingerprint
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	retry := testOptions(t, path)
	retry.OutputDir = o.OutputDir
	retry.IndexDir = o.IndexDir
	if err := retry.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if retry.report.Fingerprint != fingerprint {
		t.Fatal("inventory fingerprint unstable")
	}
	if err := retry.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\r\n")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	changed := testOptions(t, path)
	changed.OutputDir = o.OutputDir
	changed.IndexDir = o.IndexDir
	if err := changed.Run(t.Context()); err == nil {
		t.Fatal("changed source allowed on restart")
	}
	if err := verifyCompletion(filepath.Join(o.OutputDir, "2023"), fingerprint); err != nil {
		t.Fatal("completed year was damaged", err)
	}
}

func TestCancellation(t *testing.T) {
	o := testOptions(t, fixtureFile(t, full(t, "a", "2023-01-01T00:00:00Z")))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := o.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.OutputDir, "2023")); !os.IsNotExist(err) {
		t.Fatal("cancelled year published")
	}
}

func TestMetadataAuditAndUTCYear(t *testing.T) {
	a := full(t, "a", "2023-01-01T00:30:00+01:00")
	a.WarcHeader().Set(gowarc.WarcPayloadDigest, "sha256:"+strings.Repeat("0", 64))
	source := fixtureFile(t, a)
	o := testOptions(t, source)
	o.Audit = true
	if err := o.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if yr := o.report.Periods["2022"]; yr == nil || yr.Unchecked != 1 || yr.Verified != 0 {
		t.Fatal("audit/year accounting", o.report)
	}
	verified := testOptions(t, source)
	verified.Audit = true
	verified.VerifyPayloads = true
	if err := verified.Run(t.Context()); err == nil {
		t.Fatal("verification accepted invalid digest")
	}
}

func TestExactURIDateResolution(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint(ambiguous), func(t *testing.T) {
			a := full(t, "a", "2022-01-01T00:00:00Z")
			b := revisit(t, "b", "2023-01-01T00:00:00Z", a)
			b.WarcHeader().Delete(gowarc.WarcRefersTo)
			records := []gowarc.WarcRecord{b, a}
			if ambiguous {
				other := full(t, "other", "2022-01-01T00:00:00Z")
				other.WarcHeader().Set(gowarc.WarcTargetURI, a.WarcHeader().Get(gowarc.WarcTargetURI))
				records = append(records, other)
			}
			o := testOptions(t, fixtureFile(t, records...))
			err := o.Run(t.Context())
			if ambiguous && err == nil {
				t.Fatal("ambiguous URI/date accepted")
			}
			if !ambiguous && err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetainContextRecordsAndResources(t *testing.T) {
	var records []gowarc.WarcRecord
	for i, typ := range []gowarc.RecordType{gowarc.Warcinfo, gowarc.Request, gowarc.Metadata, gowarc.Resource, gowarc.Resource} {
		b := gowarc.NewRecordBuilder(typ, gowarc.WithAddMissingDigest(true))
		b.AddWarcHeader(gowarc.WarcRecordID, fmt.Sprintf("<urn:uuid:context-%d>", i))
		b.AddWarcHeader(gowarc.WarcDate, "2023-01-01T00:00:00Z")
		b.AddWarcHeader(gowarc.WarcTargetURI, "https://example.org/")
		b.AddWarcHeader(gowarc.ContentType, "application/octet-stream")
		if _, err := b.WriteString(testPayload); err != nil {
			t.Fatal(err)
		}
		wr, _, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		defer testcheck.Close(t, wr)
		records = append(records, wr)
	}
	records[2].WarcHeader().SetId(gowarc.WarcConcurrentTo, "urn:uuid:outside")
	o := testOptions(t, fixtureFile(t, records...))
	if err := o.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries := readManifest(t, o.OutputDir, "2023")
	if len(entries) != 5 {
		t.Fatal("lost context records")
	}
	for _, e := range entries[:3] {
		if e.Action != "copy" {
			t.Fatal("context record deduplicated", e)
		}
	}
	if entries[3].Action != "full" || entries[4].Action != "revisit" {
		t.Fatal("resource deduplication", entries)
	}
	if len(o.report.Periods["2023"].Findings) == 0 {
		t.Fatal("missing contextual-reference finding")
	}
}

func TestOutputFailureAndResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook fixture")
	}
	a := full(t, "a", "2022-01-01T00:00:00Z")
	b := revisit(t, "b", "2023-01-01T00:00:00Z", a)
	source := fixtureFile(t, a, b)
	o := testOptions(t, source)
	hookDir := t.TempDir()
	hook := filepath.Join(hookDir, "hook")
	// The second close fails once. Completed 2022 must remain available.
	script := "#!/bin/sh\nif [ -f '" + hookDir + "/first' ] && [ ! -f '" + hookDir + "/failed' ]; then touch '" + hookDir + "/failed'; exit 1; fi\ntouch '" + hookDir + "/first'\n"
	if err := os.WriteFile(hook, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	config, err := warcwriterconfig.New("redupe", warcwriterconfig.WithOutDir(o.OutputDir), warcwriterconfig.WithCloseOutputFileHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	o.WriterConfig = config
	if err := o.Run(t.Context()); err == nil {
		t.Fatal("close failure ignored")
	}
	if _, err := os.Stat(filepath.Join(o.OutputDir, "2022", "completion.json")); err != nil {
		t.Fatal("completed year lost", err)
	}
	if _, err := os.Stat(filepath.Join(o.OutputDir, "2023")); !os.IsNotExist(err) {
		t.Fatal("failed year published")
	}
	staging, err := filepath.Glob(filepath.Join(o.OutputDir, ".redupe-*"))
	if err != nil || len(staging) != 0 {
		t.Fatal("staging leaked", staging, err)
	}
	fingerprint := o.report.Fingerprint
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	retry := testOptions(t, source)
	retry.OutputDir = o.OutputDir
	retry.IndexDir = o.IndexDir
	retry.WriterConfig, err = warcwriterconfig.New("redupe", warcwriterconfig.WithOutDir(o.OutputDir), warcwriterconfig.WithCloseOutputFileHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	if err := retry.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := verifyCompletion(filepath.Join(o.OutputDir, "2022"), fingerprint); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveInputAndCrossFileAliases(t *testing.T) {
	a := full(t, "a", "2023-01-01T00:00:00Z")
	source := fixtureFile(t, a)
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "inputs.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for _, name := range []string{"one/input.warc", "two/input.warc"} {
		member, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(z.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
	o := testOptions(t, path)
	o.FileWalker.Recursive = true
	if err := o.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if o.report.Periods["2023"].Aliases != 1 {
		t.Fatal("archive members not cataloged separately")
	}
}

func TestCrossFileConflictsAndInvalidDates(t *testing.T) {
	a := full(t, "a", "2023-01-01T00:00:00Z")
	b := full(t, "a", "2023-02-01T00:00:00Z")
	o := testOptions(t, fixtureFile(t, a), fixtureFile(t, b))
	if err := o.Run(t.Context()); err == nil {
		t.Fatal("cross-file conflict accepted")
	}
	a.WarcHeader().Set(gowarc.WarcDate, "invalid")
	invalid := testOptions(t, fixtureFile(t, a))
	invalid.Audit = true
	if err := invalid.Run(t.Context()); err == nil {
		t.Fatal("invalid date accepted")
	}
}

func TestMissingAndDifferentlyEncodedDigests(t *testing.T) {
	a := full(t, "a", "2023-01-01T00:00:00Z")
	b := full(t, "b", "2023-02-01T00:00:00Z")
	a.WarcHeader().Delete(gowarc.WarcPayloadDigest)
	sum := sha1.Sum([]byte(testPayload))
	b.WarcHeader().Set(gowarc.WarcPayloadDigest, "sha1:"+base32.StdEncoding.EncodeToString(sum[:]))
	o := testOptions(t, fixtureFile(t, b, a))
	if err := o.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	manifest := readManifest(t, o.OutputDir, "2023")
	if manifest[0].Action != "full" || manifest[1].Action != "revisit" {
		t.Fatal("digest metadata affected identity", manifest)
	}
}

func TestWriteFailureDoesNotPublish(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook fixture")
	}
	o := testOptions(t, fixtureFile(t, full(t, "a", "2023-01-01T00:00:00Z")))
	hook := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nmkdir \"$WARC_FILE_NAME\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	o.WriterConfig, err = warcwriterconfig.New("redupe", warcwriterconfig.WithOutDir(o.OutputDir), warcwriterconfig.WithOpenOutputFileHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(t.Context()); err == nil {
		t.Fatal("writer failure ignored")
	}
	entries, err := os.ReadDir(o.OutputDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed output left behind", entries, err)
	}
}

func TestMinimumGainAndConfigurationChange(t *testing.T) {
	a := full(t, "a", "2023-01-01T00:00:00Z")
	b := full(t, "b", "2023-02-01T00:00:00Z")
	source := fixtureFile(t, a, b)
	o := testOptions(t, source)
	o.MinimumSizeGain = int64(len(testPayload))
	o.configuration["minSizeGain"] = o.MinimumSizeGain
	if err := o.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if o.report.Periods["2023"].Revisits != 0 {
		t.Fatal("minimum gain ignored")
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	changed := testOptions(t, source)
	changed.IndexDir = o.IndexDir
	changed.OutputDir = o.OutputDir
	if err := changed.Run(t.Context()); err == nil {
		t.Fatal("changed effective configuration allowed")
	}
}

func TestReconstructionOwnershipAnd304(t *testing.T) {
	a := full(t, "a", "2022-01-01T00:00:00Z")
	b := revisit(t, "b", "2023-01-01T00:00:00Z", a)
	rebuilt, err := loader.ReconstructIdentical(b, a)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, rebuilt)
	if err := errors.Join(a.Close(), b.Close()); err != nil {
		t.Fatal(err)
	}
	if err := verifyRecord(rebuilt); err != nil {
		t.Fatal("reconstruction borrowed input resources", err)
	}
	reader, err := payloadReader(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != testPayload {
		t.Fatal("reconstruction lost payload", err)
	}
	donor := full(t, "other", "2022-01-01T00:00:00Z")
	bad := gowarc.NewRecordBuilder(gowarc.Revisit)
	bad.AddWarcHeader(gowarc.WarcRecordID, "<urn:uuid:bad>")
	bad.AddWarcHeader(gowarc.WarcDate, "2023-01-01T00:00:00Z")
	bad.AddWarcHeader(gowarc.WarcProfile, gowarc.ProfileIdenticalPayloadDigestV1_1)
	bad.AddWarcHeader(gowarc.WarcPayloadDigest, donor.WarcHeader().Get(gowarc.WarcPayloadDigest))
	if _, err := bad.WriteString("HTTP/1.1 304 Not Modified\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	rec, _, err := bad.Build()
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, rec)
	if _, err := loader.ReconstructIdentical(rec, donor); err == nil {
		t.Fatal("attached payload to 304 headers")
	}
}

func TestChainDepthAndNoClosestFallback(t *testing.T) {
	a := full(t, "a", "2022-01-01T00:00:00Z")
	records := []gowarc.WarcRecord{a}
	for i := 1; i < 34; i++ {
		records = append(records, revisit(t, fmt.Sprint(i), fmt.Sprintf("2023-01-01T00:00:%02dZ", i), records[len(records)-1]))
	}
	o := testOptions(t, fixtureFile(t, records...))
	o.Audit = true
	if err := o.Run(t.Context()); err == nil {
		t.Fatal("excessive chain accepted")
	}
	b := revisit(t, "b", "2023-01-01T00:00:00Z", a)
	b.WarcHeader().Delete(gowarc.WarcRefersTo)
	b.WarcHeader().Set(gowarc.WarcRefersToDate, "2022-01-01T00:00:01Z")
	exact := testOptions(t, fixtureFile(t, a, b))
	exact.Audit = true
	if err := exact.Run(t.Context()); err == nil {
		t.Fatal("nearest capture used as donor")
	}
}

func TestCancellationDuringOutputHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell hook fixture")
	}
	o := testOptions(t, fixtureFile(t, full(t, "a", "2023-01-01T00:00:00Z")))
	hook := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	o.WriterConfig, err = warcwriterconfig.New("redupe", warcwriterconfig.WithOutDir(o.OutputDir), warcwriterconfig.WithOpenOutputFileHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := o.Run(ctx); err == nil {
		t.Fatal("cancelled output succeeded")
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("output hook ignored cancellation")
	}
	if _, err := os.Stat(filepath.Join(o.OutputDir, "2023")); !os.IsNotExist(err) {
		t.Fatal("cancelled output published")
	}
}

func TestIntervals(t *testing.T) {
	for _, tc := range []struct {
		interval string
		periods  []string
	}{
		{"year", []string{"2024"}},
		{"month", []string{"2024-02", "2024-03"}},
		{"day", []string{"2024-02-29", "2024-03-01", "2024-03-02"}},
	} {
		t.Run(tc.interval, func(t *testing.T) {
			a := full(t, "a", "2024-03-01T00:30:00+01:00")
			b := revisit(t, "b", "2024-03-01T00:00:00Z", a)
			c := revisit(t, "c", "2024-03-01T12:00:00Z", b)
			d := revisit(t, "d", "2024-03-02T00:00:00Z", c)
			source := fixtureFile(t, d, b, a, c)
			o := testOptions(t, source)
			o.Interval = tc.interval
			if err := o.Run(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(o.report.Periods) != len(tc.periods) {
				t.Fatalf("periods: %v", o.report.Periods)
			}
			for _, period := range tc.periods {
				report := o.report.Periods[period]
				if report == nil || !report.Completed {
					t.Fatalf("incomplete period %s", period)
				}
				entries := readManifest(t, o.OutputDir, period)
				if entries[0].Action != "full" {
					t.Fatal("period must start with full payload")
				}
				for _, entry := range entries[1:] {
					if entry.Action != "revisit" || entry.Target.TargetRecordId != entries[0].ID {
						t.Fatal("revisit must target this period's donor")
					}
				}
				isolated := filepath.Join(t.TempDir(), period)
				if err := os.CopyFS(isolated, os.DirFS(filepath.Join(o.OutputDir, period))); err != nil {
					t.Fatal(err)
				}
				if err := o.verifyPeriod(t.Context(), isolated, period); err != nil {
					t.Fatal(err)
				}
			}
			if err := o.Close(); err != nil {
				t.Fatal(err)
			}
			changed := testOptions(t, source)
			changed.IndexDir, changed.OutputDir = o.IndexDir, o.OutputDir
			changed.Interval = "day"
			if tc.interval == "day" {
				changed.Interval = "month"
			}
			if err := changed.Run(t.Context()); err == nil {
				t.Fatal("changed interval allowed restart")
			}
		})
	}
}

func TestInvalidInterval(t *testing.T) {
	o := testOptions(t, "unused.warc")
	o.Interval = "week"
	if err := o.Validate(); err == nil {
		t.Fatal("invalid interval accepted")
	}
}

func TestRecoverRecordedInventory(t *testing.T) {
	o := testOptions(t)
	o.Audit = true
	if err := o.open(t.Context()); err != nil {
		t.Fatal(err)
	}
	old := o.workDir
	unrelated := filepath.Join(o.IndexDir, "inventory-user")
	if err := os.Mkdir(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	// Release the handles as a terminated process would, preserving the ledger.
	o.workDir = ""
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	retry := testOptions(t)
	retry.Audit = true
	retry.IndexDir = o.IndexDir
	if err := retry.open(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale inventory: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal(err)
	}
}

func TestRejectSwappedCompletion(t *testing.T) {
	for _, period := range []string{"2023", "2023-02", "2023-02-01"} {
		dir := filepath.Join(t.TempDir(), period)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		c := completion{Report: Report{Fingerprint: "same"}, Period: "2024", Checksums: map[string]string{}}
		if err := writeJSON(filepath.Join(dir, "completion.json"), c); err != nil {
			t.Fatal(err)
		}
		if err := verifyCompletion(dir, "same"); err == nil {
			t.Fatalf("accepted swapped %s", period)
		}
	}
}

func TestIncompleteSourceCannotPublish(t *testing.T) {
	data, err := os.ReadFile("../../testdata/warc/single-record.warc")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "partial.warc")
	if err := os.WriteFile(source, append(data, "WARC/1.1"...), 0600); err != nil {
		t.Fatal(err)
	}
	o := testOptions(t, source)
	if err := o.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "parsed 154 of 162 bytes") {
		t.Fatalf("inventory error %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.OutputDir, "2023")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published incomplete period: %v", err)
	}
}

func TestFTPSourceHasNoLocalContainer(t *testing.T) {
	for _, root := range []string{"ftp://archive.example/", "ftp+account://reader@archive.example/"} {
		container, err := sourceContainer(filewalker.MemberLocator([]string{root}, "/input.warc"))
		if err != nil || container != nil {
			t.Fatalf("%s: container %v, error %v", root, container, err)
		}
	}
}

func TestReportCannotModifyPublishedPeriod(t *testing.T) {
	o := testOptions(t, fixtureFile(t, resource(t, "source", "2023-01-01T00:00:00Z")))
	if err := o.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	period := filepath.Join(o.OutputDir, "2023")
	before, err := directoryChecksums(period)
	if err != nil {
		t.Fatal(err)
	}
	o.ReportPath = filepath.Join(period, "manifest.jsonl")
	if err := o.Run(t.Context()); err == nil {
		t.Fatal("accepted report inside period")
	}
	after, err := directoryChecksums(period)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("period changed: %v", err)
	}
}

func TestReportPathAliases(t *testing.T) {
	o := testOptions(t, fixtureFile(t, resource(t, "source", "2023-01-01T00:00:00Z")))
	if err := o.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	period := filepath.Join(o.OutputDir, "2023")
	marker := filepath.Join(period, "manifest.jsonl")
	alias := filepath.Join(t.TempDir(), "period")
	if err := os.Symlink(period, alias); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	o.ReportPath = filepath.Join(alias, "elsewhere.json")
	if err := o.checkReportPaths(t.Context()); err == nil {
		t.Fatal("allowed report through symlink to period")
	}
	o.ReportPath = filepath.Join(o.OutputDir, "report.json")
	findings := o.ReportPath + ".findings.jsonl"
	if err := os.Link(marker, findings); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	f, err := createFindings(findings)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(marker)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("findings truncated published manifest")
	}
}

func resource(t *testing.T, id, date string) gowarc.WarcRecord {
	t.Helper()
	b := gowarc.NewRecordBuilder(gowarc.Resource, gowarc.WithAddMissingDigest(true))
	b.AddWarcHeader(gowarc.WarcRecordID, "<urn:uuid:"+id+">")
	b.AddWarcHeader(gowarc.WarcDate, date)
	b.AddWarcHeader(gowarc.WarcTargetURI, "https://example.org/"+id)
	b.AddWarcHeader(gowarc.ContentType, "application/octet-stream")
	if _, err := b.WriteString(testPayload); err != nil {
		t.Fatal(err)
	}
	wr, _, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wr.Close() })
	return wr
}
