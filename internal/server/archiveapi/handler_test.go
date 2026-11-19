// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package archiveapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestVirtualArchiveRangeAndCDX(t *testing.T) {
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		t.Run(fmt.Sprint(method), func(t *testing.T) {
			payload := []byte("0123456789abcdef")
			pack := func(name string, content []byte) []byte {
				var b bytes.Buffer
				z := zip.NewWriter(&b)
				w, e := z.CreateHeader(&zip.FileHeader{Name: name, Method: method})
				if e != nil {
					t.Fatal(e)
				}
				if _, e = w.Write(content); e != nil {
					t.Fatal(e)
				}
				if e = z.Close(); e != nil {
					t.Fatal(e)
				}
				return b.Bytes()
			}
			root := filepath.Join(t.TempDir(), "bundle.wacz")
			if e := os.WriteFile(root, pack("child.wacz", pack("archive/same.warc.gz", payload)), 0600); e != nil {
				t.Fatal(e)
			}
			locator := filewalker.MemberLocator([]string{root, "/child.wacz"}, "/archive/same.warc.gz")
			ctx := t.Context()
			db, e := cdxstore.Open(ctx, cdxstore.Options{Format: "badger", Dir: t.TempDir()})
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			generation, e := db.BeginFile(ctx, locator)
			if e != nil {
				t.Fatal(e)
			}
			capture := &cdx.Capture{
				Uri:      "https://example.org/",
				Ssu:      cdx.CanonicalKey("https://example.org/"),
				Source:   locator,
				Filename: "same.warc.gz",
				Ref:      "warcfile:" + locator + "#2",
				Offset:   2,
				Rle:      4,
				Sts:      timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
			}
			if e = db.WriteCapture(generation, capture); e != nil {
				t.Fatal(e)
			}
			container := finishMountedArchive(t, db, generation, locator)
			h := New(db, 100, 100)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/archives/cdx?url=https://example.org/", nil))
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			line := strings.SplitN(strings.TrimSpace(w.Body.String()), " ", 3)
			var fields map[string]any
			if e = json.Unmarshal([]byte(line[2]), &fields); e != nil {
				t.Fatal(e)
			}
			filename := fields["filename"].(string)
			if strings.Contains(filename, "wacz") || strings.Contains(filename, root) {
				t.Fatalf("container leaked: %s", filename)
			}
			for _, verb := range []string{"GET", "HEAD"} {
				r := httptest.NewRequestWithContext(t.Context(), verb, "/archives/"+filename, nil)
				r.Header.Set("Range", "bytes=2-5")
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 206 || w.Header().Get("Content-Range") != "bytes 2-5/16" {
					t.Fatalf("range %d %v %s", w.Code, w.Header(), w.Body.String())
				}
				if verb == "GET" && w.Body.String() != "2345" {
					t.Fatalf("wrong bytes %q", w.Body.String())
				}
				if w.Header().Get("Content-Encoding") != "" {
					t.Fatal("transport must preserve compressed archive bytes")
				}
			}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/archives/"+filename, nil)
			r.Header.Set("Range", "bytes=99-100")
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 416 {
				t.Fatalf("invalid range: %d", w.Code)
			}
			if e = os.Chtimes(root, container.Modified, container.Modified.Add(time.Minute)); e != nil {
				t.Fatal(e)
			}
			w = httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/archives/"+filename, nil))
			if w.Code != http.StatusConflict {
				t.Fatalf("changed outer container: %d", w.Code)
			}
			generation, e = db.BeginFile(ctx, locator)
			if e != nil {
				t.Fatal(e)
			}
			if e = db.FinishFile(generation, locator, int64(len(payload)), time.Now()); e != nil {
				t.Fatal(e)
			}
			w = httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/archives/"+filename, nil))
			if w.Code != 410 {
				t.Fatalf("obsolete file generation %d", w.Code)
			}
		})
	}
}

func finishMountedArchive(t *testing.T, db *cdxstore.DB, generation context.Context, locator string) filewalker.FileVersion {
	t.Helper()
	member, mounts, err := filewalker.OpenSource(locator)
	if err != nil {
		t.Fatal(err)
	}
	info, err := member.Stat()
	if err = errors.Join(err, member.Close(), mounts.Close()); err != nil {
		t.Fatal(err)
	}
	container, present, err := filewalker.ContainerVersion(locator)
	if err != nil || !present {
		t.Fatalf("container metadata present=%v error=%v", present, err)
	}
	if err = db.FinishFileVersion(generation, locator, filewalker.VersionOf(info), &container); err != nil {
		t.Fatal(err)
	}
	return container
}

func TestRangeVersionAfterCaptureDeletion(t *testing.T) {
	ctx := t.Context()
	source := filepath.Join(t.TempDir(), "source.warc")
	old := []byte("abcd")
	if err := os.WriteFile(source, old, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	db, err := cdxstore.Open(ctx, cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	generation, err := db.BeginFile(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	const original = "https://example.org/"
	capture := &cdx.Capture{
		Uri: original, Ssu: cdx.CanonicalKey(original), Source: source, Filename: filepath.Base(source),
		Sts: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), Rle: int64(len(old)),
		Ref: "warcfile:" + source + "#0",
	}
	if err = db.WriteCapture(generation, capture); err != nil {
		t.Fatal(err)
	}
	if err = db.FinishFileVersion(generation, source, filewalker.VersionOf(info), nil); err != nil {
		t.Fatal(err)
	}
	id := cdxstore.ArchiveID(source)
	archive, err := db.ArchiveSource(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(db, 100, 100)
	request := func(etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/archives/"+id+"/"+archive.Generation+"/capture.warc", nil)
		r.Header.Set("Range", "bytes=0-1")
		r.Header.Set("If-None-Match", etag)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	before := request("")
	etag := before.Header().Get("ETag")
	if before.Code != http.StatusPartialContent || before.Body.String() != "ab" || etag == "" {
		t.Fatalf("initial range: status=%d etag=%q body=%q", before.Code, etag, before.Body.String())
	}
	if w := request(etag); w.Code != http.StatusNotModified {
		t.Fatalf("unchanged conditional request: %d", w.Code)
	}
	line, err := cdx.Line(capture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Upload(ctx, bytes.NewReader(line), cdxstore.UploadOptions{Delete: true}); err != nil {
		t.Fatal(err)
	}
	lookup := httptest.NewRecorder()
	handler.ServeHTTP(lookup, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/archives/cdx?url="+original, nil))
	if lookup.Code != http.StatusOK || lookup.Body.Len() != 0 {
		t.Fatalf("capture after deletion: %d %q", lookup.Code, lookup.Body.String())
	}
	if w := request(""); w.Code != http.StatusPartialContent || w.Body.String() != "ab" || w.Header().Get("ETag") != etag {
		t.Fatalf("range after capture deletion: %d %q", w.Code, w.Body.String())
	}
	if err = os.WriteFile(source, []byte("1234"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(source, info.ModTime(), info.ModTime().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, conditional := range []string{"", etag} {
		if w := request(conditional); w.Code != http.StatusConflict || w.Header().Get("ETag") != "" {
			t.Fatalf("range after same-size replacement: %d %q", w.Code, w.Body.String())
		}
	}
}

type reindexWriter struct {
	*httptest.ResponseRecorder
	reindex func()
}

func (w *reindexWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	if w.reindex != nil {
		w.reindex()
		w.reindex = nil
	}
	return n, err
}

func TestSortedCDXRetainsGeneration(t *testing.T) {
	ctx := t.Context()
	db, err := cdxstore.Open(ctx, cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := filepath.Join(t.TempDir(), "sample.warc")
	index := func(data string, offset int64) string {
		if err := os.WriteFile(source, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		gen, err := db.BeginFile(ctx, source)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 2 {
			uri := fmt.Sprintf("https://example.org/%d", i)
			c := &cdx.Capture{Uri: uri, Ssu: cdx.CanonicalKey(uri), Source: source, Filename: "sample.warc", Ref: fmt.Sprintf("warcfile:%s#%d", source, offset+int64(i)), Offset: offset + int64(i), Rle: 1, Sts: timestamppb.New(time.Unix(0, 0))}
			if err := db.WriteCapture(gen, c); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.FinishFileVersion(gen, source, filewalker.VersionOf(info), nil); err != nil {
			t.Fatal(err)
		}
		a, err := db.ArchiveSource(ctx, cdxstore.ArchiveID(source))
		if err != nil {
			t.Fatal(err)
		}
		return VirtualFilename(cdxstore.ArchiveID(source), a.Generation, "sample.warc")
	}
	old := index("ab", 0)
	var fresh string
	w := &reindexWriter{httptest.NewRecorder(), func() { fresh = index("xyz", 1) }}
	h := New(db, 100, 100)
	query := "/archives/cdx?url=https://example.org/&matchType=prefix&sort=reverse"
	h.ServeHTTP(w, httptest.NewRequestWithContext(ctx, "GET", query, nil))
	if strings.Count(w.Body.String(), old) != 2 {
		t.Fatal(w.Body.String())
	}
	lookup := httptest.NewRecorder()
	h.ServeHTTP(lookup, httptest.NewRequestWithContext(ctx, "GET", query, nil))
	if fresh == "" || strings.Count(lookup.Body.String(), fresh) != 2 {
		t.Fatal(lookup.Body.String())
	}
	for _, name := range []string{old, fresh} {
		r := httptest.NewRequestWithContext(ctx, "GET", "/archives/"+name, nil)
		r.Header.Set("Range", "bytes=1-1")
		got := httptest.NewRecorder()
		h.ServeHTTP(got, r)
		if name == old && got.Code != 410 || name == fresh && (got.Code != 206 || got.Body.String() != "y") {
			t.Fatalf("range %d %q", got.Code, got.Body.String())
		}
	}
}

func TestArchiveScanLimitCountsUploads(t *testing.T) {
	ctx := t.Context()
	db, err := cdxstore.Open(ctx, cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	row := func(year int) string {
		return fmt.Sprintf(`org,example)/ %d0101000000 {"url":"https://example.org/","filename":"file.warc","offset":"0","length":"1"}`, year)
	}
	native, err := cdx.ParseIngestLine([]byte(row(2020)))
	if err != nil {
		t.Fatal(err)
	}
	native.Source = "file.warc"
	native.Ref = "warcfile:file.warc#0"
	g, err := db.BeginFile(ctx, native.Source)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.WriteCapture(g, native); err != nil {
		t.Fatal(err)
	}
	if err = db.FinishFile(g, native.Source, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Upload(ctx, strings.NewReader(row(2019)+"\n"+row(2021)), cdxstore.UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, order := range []string{"", "&sort=reverse", "&sort=closest&closest=2021"} {
		for _, limit := range []int{1, 3} {
			w := httptest.NewRecorder()
			New(db, limit, 10).ServeHTTP(w, httptest.NewRequestWithContext(ctx, "GET", "/archives/cdx?url=https://example.org/"+order, nil))
			r := w.Result()
			if err := r.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if r.StatusCode != 200 || r.Trailer.Get("X-Cdx-Scanned") != fmt.Sprint(limit) {
				t.Fatal(order, r.StatusCode, r.Trailer)
			}
			if limit == 1 && (w.Body.Len() != 0 || r.Trailer.Get("X-Cdx-Truncated") != "true") {
				t.Fatal(order, w.Body.String(), r.Trailer)
			}
			if limit == 3 && !strings.Contains(w.Body.String(), "20200101") {
				t.Fatal(order, w.Body.String())
			}
		}
	}
}
