// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package redupe

import (
	"bytes"
	"context"
	"encoding/json"
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
		Summary auditSummary
		Records []auditFinding
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
