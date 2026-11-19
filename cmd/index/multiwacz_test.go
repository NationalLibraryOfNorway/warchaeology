// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/captures"
)

func packageBytes(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	var names []string
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, e := z.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Store})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = w.Write(entries[n]); e != nil {
			t.Fatal(e)
		}
	}
	if e := z.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func leafPackage(t *testing.T) []byte {
	t.Helper()
	f := fixtures(t)[0]
	return packageBytes(t, map[string][]byte{
		"archive/" + f.Name:  f.Warc,
		"indexes/index.cdxj": []byte(f.Cdxj),
		"datapackage.json":   []byte(`{"title":"Leaf","custom":9007199254740993}`),
	})
}
func runPackage(t *testing.T, db *cdxstore.DB, name string) error {
	t.Helper()
	o := options(db, name)
	defer o.Close()
	return o.Run(t.Context())
}
func captureCount(t *testing.T, db *cdxstore.DB) int {
	t.Helper()
	n := 0
	if e := db.Scan(t.Context(), "", "", func(*cdx.Capture) error { n++; return nil }); e != nil {
		t.Fatal(e)
	}
	return n
}
func TestMultiWACZReopenManifestAndReplacement(t *testing.T) {
	leaf := leafPackage(t)
	// Exercise two levels of nesting and identical WARC filenames in distinct children.
	inner := packageBytes(t, map[string][]byte{"leaf.wacz": leaf})
	outer := filepath.Join(t.TempDir(), "bundle.wacz")
	write := func(second bool) {
		t.Helper()
		entries := map[string][]byte{"second.wacz": leaf, "datapackage.json": []byte(`{"profile":"multi-wacz-package","title":"Bundle","resources":[{"name":"second","path":"second.wacz"}]}`)}
		if second {
			entries["first.wacz"] = inner
			entries["second.wacz"] = leaf
			entries["datapackage.json"] = []byte(`{"profile":"multi-wacz-package","title":"Bundle","resources":[{"path":"first.wacz"},{"path":"second.wacz"}]}`)
		}
		if e := os.WriteFile(outer, packageBytes(t, entries), 0600); e != nil {
			t.Fatal(e)
		}
	}
	write(true)
	dir := t.TempDir()
	db, e := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: dir})
	if e != nil {
		t.Fatal(e)
	}
	if e = runPackage(t, db, outer); e != nil {
		t.Fatal(e)
	}
	initial := captureCount(t, db)
	if initial == 0 || initial%2 != 0 {
		t.Fatalf("captures=%d", initial)
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	db, e = cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: dir})
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	e = db.Scan(t.Context(), "", "", func(c *cdx.Capture) error {
		var source filewalker.Source
		if e := json.Unmarshal([]byte(c.GetSource()), &source); e != nil {
			return e
		}
		if len(source.Roots) < 2 {
			t.Errorf("lost nested roots: %s", c.GetSource())
		}
		record, e := (loader.FileStorageLoader{}).RecordByStorageRef(t.Context(), c.GetRef())
		if e != nil {
			return e
		}
		return record.Close()
	})
	if e != nil {
		t.Fatal(e)
	}
	h, e := captures.New(t.Context(), captures.Options{Prefix: "/archive", ReadOnly: true}, db)
	if e != nil {
		t.Fatal(e)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/archive/packages", nil))
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	decoder := json.NewDecoder(response.Body)
	packages := 0
	for {
		var p cdxstore.PackageInfo
		e = decoder.Decode(&p)
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		packages++
		if p.Locator == "" {
			t.Fatal("missing package source")
		}
		if strings.Contains(string(p.Manifest), "Leaf") && !strings.Contains(string(p.Manifest), "9007199254740993") {
			t.Fatal("manifest numeric precision lost")
		}
	}
	if packages != 4 {
		t.Fatalf("packages=%d", packages)
	}
	if e = runPackage(t, db, outer); e != nil {
		t.Fatal(e)
	}
	if captureCount(t, db) != initial {
		t.Fatal("cached import changed captures")
	}
	write(false)
	if e = runPackage(t, db, outer); e != nil {
		t.Fatal(e)
	}
	if captureCount(t, db) != initial/2 {
		t.Fatal("removed child's captures survived")
	}
	packages = 0
	if e = db.ListPackages(t.Context(), func(p cdxstore.PackageInfo) error { packages++; return nil }); e != nil {
		t.Fatal(e)
	}
	if packages != 2 {
		t.Fatalf("removed child metadata survived: %d", packages)
	}
}
func TestMultiWACZManifestErrors(t *testing.T) {
	for _, raw := range []string{`{"profile":"multi-wacz-package","resources":[{"path":"missing.wacz"}]}`, `{"resources":[{"path":"../escape.wacz"}]}`, `{"resources":[{"path":"https://example.com/child.wacz"}]}`, `{"resources":[{"path":"child.wacz"},{"path":"child.wacz"}]}`, `{broken`, `null`, `{"profile":"multi-wacz-package","resources":[{"url":"https://example.com/c.wacz?token=x"}]}`} {
		t.Run(raw, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "bundle.wacz")
			data := packageBytes(t, map[string][]byte{"child.wacz": leafPackage(t), "datapackage.json": []byte(raw)})
			if e := os.WriteFile(name, data, 0600); e != nil {
				t.Fatal(e)
			}
			db, e := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: t.TempDir()})
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			if e = runPackage(t, db, name); e == nil {
				t.Fatal("invalid manifest accepted")
			}
			if captureCount(t, db) != 0 {
				t.Fatal("invalid manifest published captures")
			}
		})
	}
}

func TestMultiWACZNestingLimit(t *testing.T) {
	data := leafPackage(t)
	for range 16 {
		data = packageBytes(t, map[string][]byte{"child.wacz": data})
	}
	name := filepath.Join(t.TempDir(), "deep.wacz")
	if e := os.WriteFile(name, data, 0600); e != nil {
		t.Fatal(e)
	}
	db, e := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = runPackage(t, db, name); e == nil || !strings.Contains(e.Error(), "nesting exceeds") {
		t.Fatalf("error=%v", e)
	}
	if captureCount(t, db) != 0 {
		t.Fatal("overdeep package published captures")
	}
}

func TestFailedMultiWACZReplacementKeepsPublishedTree(t *testing.T) {
	wacz := filepath.Join(t.TempDir(), "bundle.wacz")
	write := func(first, second []byte) {
		t.Helper()
		if err := os.WriteFile(wacz, packageBytes(t, map[string][]byte{
			"a.wacz": first, "b.wacz": second,
		}), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	leaf := leafPackage(t)
	write(leaf, leaf)
	if err := runPackage(t, db, wacz); err != nil {
		t.Fatal(err)
	}
	generations := func() []string {
		t.Helper()
		var visible []string
		if err := db.Scan(t.Context(), "", "", func(c *cdx.Capture) error {
			visible = append(visible, c.GetGeneration())
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return visible
	}
	before := generations()
	if len(before) == 0 {
		t.Fatal("expected captures in the original bundle")
	}
	write(leaf, []byte("invalid ZIP"))
	if err := runPackage(t, db, wacz); err == nil {
		t.Fatal("accepted a malformed child")
	}
	if after := generations(); !slices.Equal(before, after) {
		t.Fatalf("published generations changed from %v to %v", before, after)
	}
	write(leaf, leaf)
	if err := runPackage(t, db, wacz); err != nil {
		t.Fatal(err)
	}
	if after := generations(); len(after) != len(before) || slices.Equal(before, after) {
		t.Fatalf("complete retry did not replace generations: %v", after)
	}
}
