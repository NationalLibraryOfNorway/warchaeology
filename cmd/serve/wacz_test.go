// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/wacz"
)

func TestDirectWACZRoutes(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "read-only.wacz")
	writeDirectFixture(t, name)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Error(err)
		}
	})
	reader, err := wacz.Open(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, reader)
	options := ServeOptions{}
	handler := http.StripPrefix("/archive", options.directHandler(reader))
	request := func(method, path, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), method, "/archive"+path, nil)
		r.Header.Set("Range", "bytes=1-2")
		r.Header.Set("If-None-Match", etag)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	cdx := request(http.MethodGet, "/archives/cdx?url=https://example.com/&output=json", "")
	var result map[string]any
	if err := json.Unmarshal(cdx.Body.Bytes(), &result); err != nil || cdx.Code != http.StatusOK {
		t.Fatalf("lookup: %d %s %v", cdx.Code, cdx.Body, err)
	}
	raw, ok := result["filename"].(string)
	if !ok {
		t.Fatalf("missing virtual filename: %v", result)
	}
	path := "/archives/" + raw
	w := request(http.MethodGet, path, "")
	etag := w.Header().Get("ETag")
	if w.Code != http.StatusPartialContent || w.Body.String() != "bc" || etag == "" {
		t.Fatalf("range: %d %q %q", w.Code, w.Body, etag)
	}
	if w := request(http.MethodHead, path, ""); w.Code != http.StatusPartialContent || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %q", w.Code, w.Body)
	}
	if w := request(http.MethodGet, path, etag); w.Code != http.StatusNotModified {
		t.Fatalf("conditional range: %d", w.Code)
	}
	if w := request(http.MethodGet, "/cdx?url=https://example.com/&page=0", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("pagination: %d", w.Code)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected files: %v %v", entries, err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, info.ModTime(), info.ModTime().AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if w := request(http.MethodGet, path, etag); w.Code != http.StatusConflict {
		t.Fatalf("changed WACZ: %d", w.Code)
	}
}

func writeDirectFixture(t *testing.T, name string) {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for member, content := range map[string]string{
		"archive/a.warc":     "abcd",
		"indexes/index.cdxj": `com,example)/ 20250101000000 {"url":"https://example.com/","filename":"a.warc","offset":"0","length":"4"}` + "\n",
	} {
		entry, err := writer.Create(member)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, buf.Bytes(), 0400); err != nil {
		t.Fatal(err)
	}
}
