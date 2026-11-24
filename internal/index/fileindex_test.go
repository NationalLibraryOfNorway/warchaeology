// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"testing"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
)

func TestFileIndex_SaveAndGetFileStats(t *testing.T) {
	idx, err := NewFileIndex(t.TempDir(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)

	result := stat.NewResult("a.warc")
	result.IncrRecords()
	result.IncrDuplicates()
	result.AddError(testError("boom"))

	if err := idx.SaveFileStats("a.warc", result); err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetFileStats("a.warc")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Records() != 1 || got.Duplicates() != 1 || got.ErrorCount() != 1 {
		t.Fatalf("unexpected stats: records=%d duplicates=%d errors=%d", got.Records(), got.Duplicates(), got.ErrorCount())
	}
}

func TestFileIndex_GetMissingFileStats(t *testing.T) {
	idx, err := NewFileIndex(t.TempDir(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)

	got, err := idx.GetFileStats("missing.warc")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("expected nil result for missing key")
	}
}

type testError string

func (e testError) Error() string { return string(e) }
