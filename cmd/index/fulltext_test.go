// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fulltext"
)

func TestNestedInlineTextBackfillOnCachedPackage(t *testing.T) {
	leaf := leafPackage(t)
	// A nested package with inline text also exercises page-only metadata.
	pages := []byte("{\"format\":\"json-pages-1.0\"}\n{\"url\":\"https://example.org/\",\"title\":\"Northern archive\",\"text\":\"Blåbær and northern lights\",\"ts\":\"2026-01-01T00:00:00Z\"}\n")
	leaf = packageBytes(t, map[string][]byte{
		"child.wacz":        leaf,
		"pages/pages.jsonl": pages,
		"datapackage.json":  []byte(`{"resources":[{"path":"child.wacz"}]}`),
	})
	path := filepath.Join(t.TempDir(), "nested.wacz")
	if err := os.WriteFile(path, packageBytes(t, map[string][]byte{"leaf.wacz": leaf}), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = runPackage(t, db, path); err != nil {
		t.Fatal(err)
	}
	textDir := filepath.Join(t.TempDir(), "text")
	options := options(db, path)
	options.Fulltext = true
	options.TextDir = textDir
	defer options.Close()
	if err = options.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	idx, err := fulltext.Open(textDir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	found, err := idx.Search(t.Context(), fulltext.Request{Query: "blåbær"})
	if err != nil || found.Total != 1 {
		t.Fatalf("inline text=%+v error=%v", found, err)
	}
	hit := found.Hits[0]
	if hit.Fields["title"] != "Northern archive" {
		t.Fatalf("title missing: %+v", hit.Fields)
	}
	syncer := fulltext.Synchronizer{DB: db, Index: idx}
	if !syncer.Visible(t.Context(), hit.Fields) {
		raw, err := json.Marshal(hit.Fields)
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("inline hit not visible: %s", raw)
	}
	changed := packageBytes(t, map[string][]byte{"leaf.wacz": leaf, "pages/pages.jsonl": pages})
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncer.Sync(t.Context(), true); !errors.Is(err, cdxstore.ErrArchiveChanged) {
		t.Fatalf("rebuild after package replacement: got %v, want ErrArchiveChanged", err)
	}
}
