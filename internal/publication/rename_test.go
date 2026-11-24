// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package publication

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublication(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	output := filepath.Join(root, "output")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(stage); err != nil {
		t.Fatal(err)
	}
	if err := Rename(stage, output, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Rename(stage, output, false); err == nil {
		t.Fatal("replaced existing directory")
	}

	temp := filepath.Join(root, "report.tmp")
	report := filepath.Join(root, "report.json")
	if err := os.WriteFile(temp, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Rename(temp, report, false); err == nil {
		t.Fatal("replaced existing report without replace flag")
	}
	if err := Rename(temp, report, true); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(report)
	if err != nil || string(got) != "new" {
		t.Fatalf("report %q, %v", got, err)
	}
	if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file: %v", err)
	}
}
