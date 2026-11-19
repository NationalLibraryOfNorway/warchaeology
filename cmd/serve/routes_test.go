// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"encoding/json"
	"fmt"
	indexcmd "github.com/nationallibraryofnorway/warchaeology/v5/cmd/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/api"
	"github.com/spf13/afero"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRemovedCDXRoutes(t *testing.T) {
	for _, indexOnly := range []bool{false, true} {
		db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: t.TempDir(), IndexOnly: indexOnly})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		o := &ServeOptions{prefix: "/archive"}
		handler := o.databaseHandler(t.Context(), db)
		for _, path := range []string{"/warcserver/cdx", "/pywb/cdx"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/archive"+path+"?url=https://example.org/", nil))
			if response.Code != 404 {
				t.Fatalf("indexOnly=%v %s: got %d", indexOnly, path, response.Code)
			}
		}
		for _, path := range []string{"/cdx", "/archives/cdx"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/archive"+path+"?url=https://example.org/", nil))
			want := 200
			if indexOnly && path == "/archives/cdx" {
				want = 404
			}
			if response.Code != want {
				t.Fatalf("indexOnly=%v %s: got %d want %d", indexOnly, path, response.Code, want)
			}
		}
	}
}

func TestRawReplayRequiresOptIn(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	source, err := filepath.Abs("../../testdata/warc/convert.warc")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	db, err := cdxstore.Open(ctx, cdxstore.Options{Format: badgerFormat, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	generation, err := db.BeginFile(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	const original = "http://example.test"
	capture := &cdx.Capture{
		Uri: original, Ssu: cdx.CanonicalKey(original), Source: source, Filename: filepath.Base(source),
		Sts: timestamppb.New(time.Date(2025, 3, 12, 16, 26, 52, 0, time.UTC)), Rle: info.Size(),
		Ref: "warcfile:" + source + "#0", Srt: "response",
	}
	if err = db.WriteCapture(generation, capture); err != nil {
		t.Fatal(err)
	}
	if err = db.FinishFile(generation, source, info.Size(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		options := ServeOptions{prefix: "/archive", allowSameOriginReplay: enabled}
		handler := options.databaseHandler(ctx, db)
		w := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/archive/warcserver/web/20250312162652id_/"+original, nil)
		handler.ServeHTTP(w, r)
		want := http.StatusNotFound
		if enabled {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Fatalf("opted in=%v: got status %d, want %d (%s)", enabled, w.Code, want, w.Body.String())
		}
	}
}

func TestReplayRedirectToClosestCapture(t *testing.T) {
	const destination = "https://example.org/target?q=1"
	var records strings.Builder
	for i, capture := range []struct{ uri, date, block string }{
		{"http://example.org/target?q=1", "2020-01-01", "HTTP/1.1 302 Found\r\nLocation: " + destination + "\r\n\r\n"},
		{destination, "2020-01-01", "HTTP/1.1 200 OK\r\n\r\nfirst"},
		{destination, "2021-01-01", "HTTP/1.1 200 OK\r\n\r\nsecond"},
		{"https://example.org/relative", "2020-01-01", "HTTP/1.1 303 See Other\r\nLocation: /target?q=1\r\n\r\n"},
		{"https://example.org/absolute", "2020-01-01", "HTTP/1.1 303 See Other\r\nLocation: " + destination + "\r\n\r\n"},
		{"https://example.org/missing", "2020-01-01", "HTTP/1.1 303 See Other\r\nLocation: /absent\r\n\r\n"},
	} {
		fmt.Fprintf(&records, "WARC/1.1\r\nWARC-Type: response\r\nWARC-Record-ID: <urn:uuid:20add669-b781-4c66-977c-%012d>\r\nWARC-Date: %sT00:00:00Z\r\nWARC-Target-URI: %s\r\nContent-Type: application/http; msgtype=response\r\nContent-Length: %d\r\n\r\n%s\r\n\r\n", i, capture.date, capture.uri, len(capture.block), capture.block)
	}
	source := filepath.Join(t.TempDir(), "redirect.warc")
	if err := os.WriteFile(source, []byte(records.String()), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: badgerFormat, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	ingest := &indexcmd.IndexOptions{Paths: []string{source}, Sink: db, Concurrency: 1, FileWalker: filewalker.New(filewalker.WithFs(afero.NewOsFs()))}
	defer testcheck.Close(t, ingest)
	if err := ingest.Run(t.Context()); err != nil {
		t.Fatal(err)
	}

	handler := (&ServeOptions{prefix: "/archive", allowSameOriginReplay: true}).databaseHandler(t.Context(), db)
	const base = "http://example.test/archive/warcserver/web/"
	for _, timestamp := range []string{"20200101000000id_", "2020id_", "20200101000000"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, base+timestamp+"/http://example.org/target?q=1", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusFound || w.Header().Get("Location") != base+"20200101000000id_/"+destination {
			t.Fatalf("%s: status %d, Location %q", timestamp, w.Code, w.Header().Get("Location"))
		}
		follow := httptest.NewRecorder()
		handler.ServeHTTP(follow, httptest.NewRequestWithContext(t.Context(), http.MethodGet, w.Header().Get("Location"), nil))
		if follow.Code != http.StatusOK || follow.Body.String() != "first" {
			t.Errorf("redirect destination returned %d %q", follow.Code, follow.Body.String())
		}
	}
	for _, path := range []string{"relative", "absolute", "missing"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, base+"2020id_/https://example.org/"+path, nil))
		if path == "missing" {
			if w.Code != http.StatusNotFound {
				t.Fatalf("missing target: %d", w.Code)
			}
			continue
		}
		if w.Header().Get("Location") != base+"20200101000000id_/"+destination || w.Code != http.StatusFound {
			t.Fatalf("%s: %d %v", path, w.Code, w.Header())
		}
	}

}

// cancelOnStat cancels the parent after the server has started ingestion.
type cancelOnStat struct {
	afero.Fs
	cancel context.CancelFunc
}

func (f cancelOnStat) Stat(path string) (os.FileInfo, error) {
	f.cancel()
	return nil, context.Canceled
}

func TestShutdownDuringIngestion(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	db, err := cdxstore.Open(ctx, cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	walker := filewalker.New(filewalker.WithFs(cancelOnStat{afero.NewMemMapFs(), cancel}))
	opts := &indexcmd.IndexOptions{Paths: []string{"input.warc"}, FileWalker: walker, Sink: db, Concurrency: 1}
	defer testcheck.Close(t, walker)
	if err := run(ctx, "127.0.0.1:0", http.NotFoundHandler(), opts); err != nil {
		t.Fatal(err)
	}
}

func TestNativeEscapedCaptures(t *testing.T) {
	const block = "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"
	paths := []string{"file%20one", "caf%C3%A9", "one%2Ftwo?q=b&a=1"}
	var records strings.Builder
	offsets := make(map[string]int)
	for i, path := range paths {
		id := fmt.Sprintf("urn:uuid:20add669-b781-4c66-977c-%012d", i)
		offsets[id] = records.Len()
		fmt.Fprintf(&records, "WARC/1.1\r\nWARC-Type: response\r\nWARC-Record-ID: <%s>\r\nWARC-Date: 2022-01-01T00:00:00Z\r\nWARC-Target-URI: https://example.org/%s\r\nContent-Type: application/http; msgtype=response\r\nContent-Length: %d\r\n\r\n%s\r\n\r\n", id, path, len(block), block)
	}
	source := filepath.Join(t.TempDir(), "capture#one.warc")
	if err := os.WriteFile(source, []byte(records.String()), 0600); err != nil {
		t.Fatal(err)
	}
	opts := cdxstore.Options{Format: badgerFormat, Dir: t.TempDir()}
	db, err := cdxstore.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	ingest := &indexcmd.IndexOptions{Paths: []string{source}, Sink: db, Concurrency: 1, FileWalker: filewalker.New(filewalker.WithFs(afero.NewOsFs()))}
	defer testcheck.Close(t, ingest)
	if err := ingest.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = cdxstore.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	handler := (&ServeOptions{allowSameOriginReplay: true}).databaseHandler(t.Context(), db)
	get := func(path string) string {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	for _, path := range paths {
		if body := get("/warcserver/web/2022id_/https://example.org/" + path); body != "hello" {
			t.Errorf("replayed %q", body)
		}
	}
	ids := json.NewDecoder(strings.NewReader(get("/id")))
	for ids.More() {
		var row struct {
			ID, Filename string
			Offset       int
		}
		if err := ids.Decode(&row); err != nil {
			t.Fatal(err)
		}
		offset, ok := offsets[row.ID]
		if !ok || row.Offset != offset || !strings.Contains(row.Filename, "capture#one.warc") {
			t.Fatalf("unexpected row %+v", row)
		}
		delete(offsets, row.ID)
	}
	if len(offsets) != 0 {
		t.Fatalf("missing IDs %v", offsets)
	}
	req, err := api.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := db.CreateReport(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for report.GetStatus() != schema.Report_COMPLETED {
		if time.Now().After(deadline) || report.GetStatus() == schema.Report_FAILED {
			t.Fatalf("report %v", report)
		}
		time.Sleep(time.Millisecond)
		report, err = db.GetReport(t.Context(), report.GetId())
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := report.GetData().GetPayloadLength(); n != 15 {
		t.Fatalf("payload bytes %d, want 15", n)
	}
}
