// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package archiveapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
				_, _ = w.Write(content)
				if e = z.Close(); e != nil {
					t.Fatal(e)
				}
				return b.Bytes()
			}
			root := filepath.Join(t.TempDir(), "bundle.wacz")
			if e := os.WriteFile(root, pack("child.wacz", pack("archive/same.warc.gz", payload)), 0600); e != nil {
				t.Fatal(e)
			}
			locator, _ := json.Marshal(filewalker.Source{Roots: []string{root, "/child.wacz"}, Path: "/archive/same.warc.gz"})
			ctx := context.Background()
			db, e := cdxstore.Open(ctx, cdxstore.Options{Format: "badger", Dir: t.TempDir()})
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			generation, e := db.BeginFile(ctx, string(locator))
			if e != nil {
				t.Fatal(e)
			}
			capture := &cdx.Capture{
				Uri:      "https://example.org/",
				Ssu:      cdx.CanonicalKey("https://example.org/"),
				Source:   string(locator),
				Filename: "same.warc.gz",
				Ref:      "warcfile:" + string(locator) + "#2",
				Offset:   2,
				Rle:      4,
				Sts:      timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
			}
			if e = db.WriteCapture(generation, capture); e != nil {
				t.Fatal(e)
			}
			if e = db.FinishFile(generation, string(locator), int64(len(payload)), time.Time{}); e != nil {
				t.Fatal(e)
			}
			h := New(db, 100, 100)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/archives/cdx?url=https://example.org/", nil))
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
				r := httptest.NewRequest(verb, "/archives/"+filename, nil)
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
			r := httptest.NewRequest("GET", "/archives/"+filename, nil)
			r.Header.Set("Range", "bytes=99-100")
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 416 {
				t.Fatalf("invalid range: %d", w.Code)
			}
			generation, e = db.BeginFile(ctx, string(locator))
			if e != nil {
				t.Fatal(e)
			}
			if e = db.FinishFile(generation, string(locator), int64(len(payload)), time.Now()); e != nil {
				t.Fatal(e)
			}
			w = httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/archives/"+filename, nil))
			if w.Code != 410 {
				t.Fatalf("obsolete file generation %d", w.Code)
			}
		})
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
