// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package redupe

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"github.com/nationallibraryofnorway/gowarc/v4"
	"os"
	"path/filepath"
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
	o := auditOptions{originals: originals}
	if err := o.run(context.Background(), root, &out); err == nil {
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
	if err := o.run(context.Background(), root, &out); err != nil {
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
			err = o.run(context.Background(), root, &out)
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
