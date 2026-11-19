// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package fulltext

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
)

func TestPublishSearchReopenAndDelete(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "text")
	idx, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	docs := []Document{
		{ID: "a", URL: "https://example.org/", Host: "example.org", Text: "Senja blåbær", Package: "one"},
		{ID: "b", URL: "https://example.org/", Host: "example.org", Text: "Senja blåbær", Package: "two"},
	}
	if err := idx.Publish(ctx, docs, nil); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	idx, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)
	r, err := idx.Search(ctx, Request{Query: "BLÅBÆR", Package: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 1 || r.Hits[0].ID != "b" {
		t.Fatalf("unexpected results: %+v", r)
	}
	if err := idx.Publish(ctx, nil, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	r, err = idx.Search(ctx, Request{Query: "Senja"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 1 {
		t.Fatalf("got %d hits", r.Total)
	}
}

func TestReadOnlyExistingIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index")
	idx, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = idx.Publish(t.Context(), []Document{{ID: "one", URL: "https://example.org/", Text: "Senja"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}
	idx, err = OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)
	result, err := idx.Search(t.Context(), Request{Query: "Senja"})
	if err != nil || result.Total != 1 {
		t.Fatalf("read-only search %+v %v", result, err)
	}
}
