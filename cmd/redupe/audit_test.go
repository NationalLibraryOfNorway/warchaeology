// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package redupe

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/v2"
	"fmt"
	"github.com/nationallibraryofnorway/gowarc/v4"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func auditFixture(t *testing.T) (root, backup, originals string) {
	t.Helper()
	root, backup, originals = t.TempDir(), t.TempDir(), t.TempDir()
	date := "2021-01-02T03:04:05Z"
	a := full(t, "a", date)
	b := revisit(t, "b", date, a)
	c := revisit(t, "c", date, b)
	if err := os.Mkdir(filepath.Join(root, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	for path, src := range map[string]string{filepath.Join(root, "nested", "b.warc"): fixtureFile(t, b), filepath.Join(root, "c.warc"): fixtureFile(t, c), filepath.Join(backup, "lost.warc"): fixtureFile(t, a), filepath.Join(originals, "original.warc"): fixtureFile(t, full(t, "b", date), full(t, "c", date))} {
		if err := os.Rename(src, path); err != nil {
			t.Fatal(err)
		}
	}
	return root, backup, originals
}
func TestCollectionAudit(t *testing.T) {
	root, backup, originals := auditFixture(t)
	var out bytes.Buffer
	audit := func() error {
		cmd := NewCmdAudit()
		cmd.SetOut(&out)
		cmd.SetArgs([]string{root, "--originals", originals})
		return cmd.Execute()
	}
	if err := audit(); err == nil {
		t.Fatal("missing donor passed")
	}
	var report struct {
		Summary auditSummary   `json:"summary"`
		Records []auditFinding `json:"records"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Summary.Missing != 1 || report.Summary.Counts["directly-dangling"] != 1 || report.Summary.Counts["transitively-dangling"] != 1 {
		t.Fatalf("%+v", report)
	}
	if err := os.Rename(filepath.Join(backup, "lost.warc"), filepath.Join(root, "lost.warc")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := audit(); err != nil {
		t.Fatalf("%v %s", err, out.String())
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Summary.Counts["original-verified"] != 2 {
		t.Fatalf("%v %s", err, out.String())
	}
}

func TestCollectionRecovery(t *testing.T) {
	for _, mode := range []string{"backup", "original", "unavailable", "conflicting-backup"} {
		t.Run(mode, func(t *testing.T) {
			root, backup, originals := auditFixture(t)
			before, err := os.ReadFile(filepath.Join(root, "nested", "b.warc"))
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), "recovered")
			o := auditOptions{repair: dst}
			switch mode {
			case "backup", "conflicting-backup":
				o.backups = []string{backup}
				if mode == "conflicting-backup" {
					dup := full(t, "a", "2022-01-02T03:04:05Z")
					if err := os.Rename(fixtureFile(t, dup), filepath.Join(backup, "conflict.warc")); err != nil {
						t.Fatal(err)
					}
				}
			case "original":
				o.originals = originals
			}
			var out bytes.Buffer
			cmd := NewCmdRecover()
			args := []string{root, "--output-dir", dst}
			if mode == "original" {
				cmd = NewCmdAudit()
				args = []string{root, "--repair-to", dst, "--originals", originals}
			}
			for _, p := range o.backups {
				args = append(args, "--recovery-source", p)
			}
			cmd.SetOut(&out)
			cmd.SetArgs(args)
			err = cmd.Execute()
			if mode == "unavailable" || mode == "conflicting-backup" {
				if err == nil {
					t.Fatal("unverified recovery published")
				}
				if _, e := os.Stat(dst); !os.IsNotExist(e) {
					t.Fatal(e)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v %s", err, out.String())
			}
			after, err := os.ReadFile(filepath.Join(root, "nested", "b.warc"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("source changed", err)
			}
			if err := os.Rename(backup, backup+".offline"); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(originals, originals+".offline"); err != nil {
				t.Fatal(err)
			}
			if err := (auditOptions{}).run(context.Background(), dst, &out); err != nil {
				t.Fatal(err)
			}
			if mode == "backup" {
				copied, err := os.ReadFile(filepath.Join(dst, "nested", "b.warc"))
				if err != nil || !bytes.Equal(before, copied) {
					t.Fatal("unaffected file changed", err)
				}
			}
		})
	}
}

func TestAuditStaleOriginal(t *testing.T) {
	date := "2021-01-02T03:04:05Z"
	donor := full(t, "donor", date)
	lost := fullWithPayload(t, "revisit", date, "different original body")
	lost.WarcHeader().Set(gowarc.WarcPayloadDigest, donor.WarcHeader().Get(gowarc.WarcPayloadDigest))
	rv := revisit(t, "revisit", date, donor)
	source := filepath.Dir(fixtureFile(t, donor, rv))
	originals := filepath.Dir(fixtureFile(t, lost))
	var out bytes.Buffer
	run := func(o auditOptions, expected string, success bool) {
		t.Helper()
		out.Reset()
		err := o.run(context.Background(), source, &out)
		if (err == nil) != success || !bytes.Contains(out.Bytes(), []byte(expected)) {
			t.Fatalf("%v %s", err, out.String())
		}
	}
	run(auditOptions{}, "output-consistent", true)
	run(auditOptions{originals: originals}, "original-mismatch", false)
	run(auditOptions{originals: originals, repair: filepath.Join(t.TempDir(), "fixed")}, "restored-original-corrected-digest", true)
}

func TestAuditIncomplete(t *testing.T) {
	root, _, _ := auditFixture(t)
	if err := os.WriteFile(filepath.Join(root, "bad.warc.gz"), []byte{31, 139, 8, 0}, 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (auditOptions{}).run(context.Background(), root, &out); err == nil {
		t.Fatal("incomplete audit passed")
	}
	var r struct {
		Summary auditSummary `json:"summary"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Summary.Complete || r.Summary.Counts["unreadable"] != 1 || r.Summary.Revisits != 2 {
		t.Fatal(out.String())
	}
}

func TestAuditCandidateIsNeverSubstituted(t *testing.T) {
	root, _, _ := auditFixture(t)
	copy := full(t, "unrelated", "2020-01-02T03:04:05Z")
	if err := os.Rename(fixtureFile(t, copy), filepath.Join(root, "copy.warc")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	dst := filepath.Join(t.TempDir(), "fixed")
	if err := (auditOptions{repair: dst}).run(context.Background(), root, &out); err == nil {
		t.Fatal("digest-only repair accepted")
	}
	var r struct {
		Summary auditSummary `json:"summary"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Summary.Counts["digest-only-candidate"] != 1 || r.Summary.Verified.Missing != 1 {
		t.Fatal(out.String())
	}
}

func TestAuditExactURIDateRecovery(t *testing.T) {
	date := "2022-09-11T12:00:00Z"
	donor := full(t, "donor", date)
	rv := revisit(t, "later", date, donor)
	rv.WarcHeader().Delete(gowarc.WarcRefersTo)
	root := filepath.Dir(fixtureFile(t, rv))
	backup := filepath.Dir(fixtureFile(t, donor))
	var out bytes.Buffer
	if err := (auditOptions{repair: filepath.Join(t.TempDir(), "fixed"), backups: []string{backup}}).run(context.Background(), root, &out); err != nil {
		t.Fatal(err, out.String())
	}
}

func TestAuditMalformedRevisitDate(t *testing.T) {
	const date = "2023-01-02T03:04:05Z"
	donor := full(t, "a", date)
	link := revisit(t, "b", date, donor)
	link.WarcHeader().Set(gowarc.WarcRefersToDate, "invalid")
	src := fixtureFile(t, donor, link)
	var out bytes.Buffer
	if err := (auditOptions{}).run(t.Context(), filepath.Dir(src), &out); err == nil {
		t.Fatal("certified invalid revisit")
	}
	var report struct {
		Summary auditSummary   `json:"summary"`
		Records []auditFinding `json:"records"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Summary.Complete || report.Summary.Counts["unverifiable"] != 1 || len(report.Records) != 1 || report.Records[0].Detail != invalidRevisitDate {
		t.Fatalf("%s", out.Bytes())
	}
}

// The fixture contains the response and revisit emitted by b385e3ca dedup --repair.
func TestLegacyChunkRecovery(t *testing.T) {
	f, err := os.Open("testdata/v3-chunked.warc.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	split := strings.LastIndex(string(data), "WARC/1.1\r\n")
	original, link := string(data[:split]), string(data[split:])
	const legacy = "e3959d41fc81f8cae9aa6a175c5527fa4f5f5c9a393f5429897a76f2ac19843b"
	const decoded = "6896d9ea3f73a4434f5832bc65714e7d066f177373f36f34dc8a6f735daa41b1"
	for _, mode := range []string{"legacy", "partial", "correct", "unrelated", "damaged"} {
		t.Run(mode, func(t *testing.T) {
			donor := original
			first := strings.Replace(link, "WARC-Date: 2026", "WARC-Date: 2027", 1)
			switch mode {
			case "partial", "correct":
				donor = strings.Replace(donor, legacy, decoded, 1)
			case "damaged":
				donor = strings.Replace(donor, "AAAAAAAA", "BAAAAAAA", 1)
			case "unrelated":
				first = strings.Replace(first, legacy, strings.Repeat("0", 64), 1)
			}
			if mode == "correct" {
				first = strings.Replace(first, legacy, decoded, 1)
			}
			chain := strings.NewReplacer("000000000002>", "000000000003>", "000000000001>", "000000000002>", "https://example.org/2", "https://example.org/3", "https://example.org/1", "https://example.org/2", "WARC-Refers-To-Date: 2026-01-01", "WARC-Refers-To-Date: 2027-01-02").Replace(first)
			sibling := strings.ReplaceAll(first, "000000000002>", "000000000004>")
			root := t.TempDir()
			for name, contents := range map[string]string{"a": donor, "b": first, "c": chain, "d": sibling} {
				if err := os.WriteFile(filepath.Join(root, name+".warc"), []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			strict := testOptions(t, root)
			strict.Audit = true
			strict.VerifyPayloads = true
			if err := strict.Run(t.Context()); (err == nil) != (mode == "correct") {
				t.Fatalf("initial verification: %v", err)
			}
			dst := filepath.Join(t.TempDir(), "fixed")
			var report bytes.Buffer
			err := (auditOptions{repair: dst}).run(t.Context(), root, &report)
			if mode == "unrelated" || mode == "damaged" {
				if err == nil {
					t.Fatal("published damaged collection")
				}
				if _, err := os.Stat(dst); !os.IsNotExist(err) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v %s", err, report.String())
			}
			// Replay a second-generation revisit from the recovered collection.
			collection, err := openInventory(t.Context(), t.TempDir(), dst)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := collection.scratch.Close(); err != nil {
					t.Error(err)
				}
			})
			entry, err := collection.byID(t.Context(), "urn:uuid:20add669-b781-4c66-977c-000000000003")
			if err != nil || entry == nil {
				t.Fatalf("lookup: %v", err)
			}
			record, err := collection.reconstruct(t.Context(), entry)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := record.Close(); err != nil {
					t.Error(err)
				}
			})
			block := record.Block().(gowarc.HttpResponseBlock)
			body, err := block.PayloadBytes()
			if err != nil {
				t.Fatal(err)
			}
			entity, err := io.ReadAll(body)
			if err != nil || string(entity) != strings.Repeat("A", 4096) || string(block.ProtocolHeaderBytes()) != "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" {
				t.Fatalf("replayed capture changed: %v", err)
			}
			for _, verify := range []bool{false, true} {
				o := testOptions(t, dst)
				o.Audit = true
				o.VerifyPayloads = verify
				if err := o.Run(t.Context()); err != nil {
					t.Fatal(err)
				}
				if y := o.report.Periods["2027"]; y == nil || y.Expansion != 4096 {
					t.Fatalf("decoded expansion: %+v", y)
				}
			}
		})
	}
}

func TestAuditResourceRevisitFraming(t *testing.T) {
	const mime = "application/http; msgtype=response"
	body := strings.Repeat("hello", 500)
	message := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	httpDonor := resourceCapture(t, "first", mime, message)
	plainDonor := resourceCapture(t, "plain", "text/plain", body)
	httpCapture := resourceCapture(t, "second", mime, message)
	plainCapture := resourceCapture(t, "third", "text/plain", body)
	for _, tc := range []struct {
		name           string
		donor, capture gowarc.WarcRecord
	}{
		{"http-to-http", httpDonor, httpCapture},
		{"http-to-plain", httpDonor, plainCapture},
		{"plain-to-http", plainDonor, httpCapture},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := testOptions(t, fixtureFile(t, tc.donor, tc.capture))
			if err := o.Run(t.Context()); err != nil {
				t.Fatal(err)
			}
			if year := o.report.Periods["2022"]; year == nil || !year.Completed || year.Revisits != 1 {
				t.Fatalf("expected one published revisit: %+v", year)
			}
			var report bytes.Buffer
			if err := (auditOptions{}).run(t.Context(), o.OutputDir, &report); err != nil || !strings.Contains(report.String(), "output-consistent") {
				t.Fatalf("audit: %v %s", err, &report)
			}
		})
	}
}

func TestAuditResponseRevisitLength(t *testing.T) {
	const date = "2021-01-02T03:04:05Z"
	donor := fullWithPayload(t, "donor", date, strings.Repeat("A", 4096))
	shorter := fullWithPayload(t, "short", date, strings.Repeat("A", 4095))
	ref, err := donor.CreateRevisitRef(gowarc.ProfileIdenticalPayloadDigestV1_1)
	if err != nil {
		t.Fatal(err)
	}
	link, err := shorter.ToRevisitRecord(ref)
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	link.WarcHeader().Set(gowarc.WarcPayloadDigest, donor.WarcHeader().Get(gowarc.WarcPayloadDigest))
	root := filepath.Dir(fixtureFile(t, donor, link))
	dst := filepath.Join(t.TempDir(), "recovered")
	for _, o := range []auditOptions{{}, {repair: dst}} {
		var report bytes.Buffer
		if err := o.run(t.Context(), root, &report); err == nil || !strings.Contains(report.String(), "response revisit expects 4095 payload bytes, donor supplied 4096") {
			t.Fatalf("audit returned %v: %s", err, &report)
		}
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("recovery destination: %v", err)
	}
}

func TestLegacyChunkedRequestResourceRecovery(t *testing.T) {
	const header = "POST /submit HTTP/1.1\r\nHost: example.org\r\nTransfer-Encoding: chunked\r\n\r\n"
	const stored = "2\r\nhe\r\n3\r\nllo\r\n0\r\n\r\n"
	const mime = "application/http; msgtype=request"
	donor := resourceCapture(t, "donor", mime, header+stored)
	capture := resourceCapture(t, "later", mime, header+stored)
	ref, err := donor.CreateRevisitRef(gowarc.ProfileIdenticalPayloadDigestV1_1)
	if err != nil {
		t.Fatal(err)
	}
	link, err := capture.ToRevisitRecord(ref)
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	for _, record := range []gowarc.WarcRecord{donor, link} {
		record.WarcHeader().Set(gowarc.WarcPayloadDigest, "sha256:"+hashString(stored))
	}
	input := fixtureFile(t, donor, link)
	root := filepath.Dir(input)
	var report bytes.Buffer
	if err := (auditOptions{}).run(t.Context(), root, &report); err == nil || !strings.Contains(report.String(), `"legacy-chunked-digest":2`) {
		t.Fatalf("audit returned %v: %s", err, &report)
	}
	dst := filepath.Join(t.TempDir(), "recovered")
	report.Reset()
	if err := (auditOptions{repair: dst}).run(t.Context(), root, &report); err != nil {
		t.Fatalf("recovery returned %v: %s", err, &report)
	}
	data, err := os.ReadFile(filepath.Join(dst, filepath.Base(input)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(header+stored)) || bytes.Count(data, []byte("WARC-Payload-Digest: sha256:"+hashString("hello")+"\r\n")) != 2 {
		t.Fatal("expected original request framing and corrected entity labels on both records")
	}
	report.Reset()
	if err := (auditOptions{}).run(t.Context(), dst, &report); err != nil || !strings.Contains(report.String(), `"output-consistent":1`) {
		t.Fatalf("recovered audit returned %v: %s", err, &report)
	}
}
