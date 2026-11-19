// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package captures

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testLine = `wrong 20200102030405 {"url":"https://example.com/","offset":0,"length":100,"filename":"a.warc.gz","extra":9007199254740993}`

func TestCollectionHTTP(t *testing.T) {
	h, e := New(t.Context(), Options{Directory: t.TempDir(), Prefix: "/api", ResultLimit: 1}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body)))
		return w
	}
	if w := request("PUT", "/collections/a", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	for _, name := range []string{"one", "two"} {
		if w := request("PUT", "/api/collections/"+name, ""); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := request("POST", "/api/collections/one/cdx", testLine); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("GET", "/api/collections/two/export", ""); w.Body.Len() != 0 {
		t.Fatal("collection leaked")
	}
	if w := request("GET", "/api/collections/one/cdx?url=https://example.com/", ""); !strings.Contains(w.Body.String(), "9007199254740993") {
		t.Fatal(w.Body.String())
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = gz.Write([]byte(strings.Replace(testLine, `"offset":0`, `"offset":1`, 1)))
	_ = gz.Close()
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/api/collections/one/cdx", &compressed)
	r.Header.Set("Content-Encoding", "gzip")
	r.ContentLength = -1
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request("GET", "/api/collections/one/cdx?urlkey=com,example)/", "")
	if w.Result().Trailer.Get("X-Cdx-Truncated") != "true" {
		t.Fatal(w.Result().Trailer)
	}
	w = request("POST", "/api/collections/one/cdx?badLines=skip", testLine+"\ninvalid")
	var response struct {
		Report string `json:"report"`
	}
	if e = json.Unmarshal(w.Body.Bytes(), &response); e != nil || response.Report == "" {
		t.Fatal(w.Body.String(), e)
	}
	if w = request("GET", response.Report, ""); !strings.Contains(w.Body.String(), `"line":2`) {
		t.Fatal(w.Body.String())
	}
	h.ReadOnly = true
	if w = request("POST", "/api/collections/one/delete", testLine); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w = request("PUT", "/api/collections/three", ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestConcurrentRetryAndDecodedLimit(t *testing.T) {
	h, e := New(t.Context(), Options{Directory: t.TempDir(), MaxBytes: int64(len(testLine))}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "PUT", "/collections/a", nil))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "POST", "/collections/a/cdx", strings.NewReader(testLine)))
			if w.Code != http.StatusOK {
				t.Errorf("%d %s", w.Code, w.Body.String())
			}
		})
	}
	wg.Wait()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/collections/a/export", nil))
	if strings.Count(w.Body.String(), "\n") != 1 {
		t.Fatal(w.Body.String())
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	_, _ = gz.Write([]byte(testLine + "\n" + testLine))
	_ = gz.Close()
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/collections/a/cdx", &b)
	r.Header.Set("Content-Encoding", "gzip")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatal("decoded limit bypassed")
	}
}
