// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	indexcmd "github.com/nationallibraryofnorway/warchaeology/v5/cmd/index"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fulltext"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/api"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/searchui"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/wacz"
	"github.com/spf13/afero"
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

func TestPlaybackURLBase(t *testing.T) {
	for _, raw := range []string{"", "http://localhost:8080/collection", "https://replay.example.org/collection/"} {
		o := ServeOptions{format: badgerFormat, playbackURLBase: raw}
		if err := o.Validate(); err != nil || o.playbackURLBase != strings.TrimSuffix(raw, "/") {
			t.Errorf("base %q: got %q, %v", raw, o.playbackURLBase, err)
		}
	}
	for _, raw := range []string{"/relative", "file:///archive", "https://", "https://user@host", "https://host/path?", "https://host/#fragment", "https://host:bad"} {
		o := ServeOptions{format: badgerFormat, playbackURLBase: raw}
		if err := o.Validate(); err == nil {
			t.Errorf("accepted invalid base %q", raw)
		}
	}
}

func TestPrefixedSearchConsole(t *testing.T) {
	ctx := t.Context()
	db, err := cdxstore.Open(ctx, cdxstore.Options{Format: badgerFormat, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	idx, err := fulltext.Open(filepath.Join(t.TempDir(), "text"))
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)
	const base = "https://replay.example.org/collection"
	o := ServeOptions{prefix: "/archive", fulltext: true}
	handler := o.withSearch(o.databaseHandler(ctx, db), searchui.New(idx, nil, base))
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/archive/"+path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w
	}
	if !strings.Contains(get("console/").Body.String(), `src="logo_small.png"`) {
		t.Fatal("missing logo in console")
	}
	logo := get("console/logo_small.png")
	if _, err := png.Decode(logo.Body); err != nil || logo.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("invalid console logo: %v, %s", err, logo.Header().Get("Content-Type"))
	}
	var status struct {
		PlaybackURLBase string `json:"playbackUrlBase"`
	}
	if err := json.Unmarshal(get("search/status").Body.Bytes(), &status); err != nil || status.PlaybackURLBase != base {
		t.Fatalf("unexpected playback settings: %+v, %v", status, err)
	}
	get("search?q=hello")
	if body := get("search/cdx?url=example.com").Body.String(); !strings.Contains(body, `"truncated":false`) {
		t.Fatal(body)
	}
	for _, path := range []string{"/search/cdx", "/archive-other/search/cdx"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, path+"?url=example.com", nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", path, w.Code)
		}
	}
	const first = "https://example.org/page"
	const second = first + "/two"
	for _, original := range []string{first, second} {
		line := cdx.CanonicalKey(original) + " 20260102030405 " +
			fmt.Sprintf(`{"url":%q,"mime":"text/html","status":"200","filename":"sample.warc","offset":"0","length":"10"}`, original)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodPost, "/archive/cdx", strings.NewReader(line)))
		if w.Code != http.StatusOK {
			t.Fatalf("upload: %d %s", w.Code, w.Body.String())
		}
	}
	for match, want := range map[string]int{"exact": 1, "prefix": 2, "host": 2, "domain": 2} {
		w := get("cdx?output=json&limit=100&url=" + first + "&matchType=" + match)
		rows := strings.FieldsFunc(w.Body.String(), func(r rune) bool { return r == '\n' })
		if len(rows) != want || !strings.Contains(rows[0], first) {
			t.Errorf("%s returned %q", match, rows)
		}
	}
}

func TestRawReplayRequiresOptIn(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	raw, err := os.ReadFile("../../testdata/warc/convert.warc")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "capture.warc")
	err = os.WriteFile(source, raw, 0600)
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
	const id = "urn:uuid:20add669-b781-4c66-977c-e50e280a69e9"
	capture := &cdx.Capture{
		Uri: original, Ssu: cdx.CanonicalKey(original), Source: source, Filename: filepath.Base(source),
		Sts: timestamppb.New(time.Date(2025, 3, 12, 16, 26, 52, 0, time.UTC)), Rle: info.Size(),
		Ref: "warcfile:" + source + "#0", Srt: "response", Rid: id,
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
	if err := os.WriteFile(source, append(bytes.Clone(raw), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	options := ServeOptions{allowSameOriginReplay: true}
	handler := options.databaseHandler(ctx, db)
	for _, path := range []string{"/warcserver/web/20250312162652id_/" + original, "/record/" + id} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
		if response.Code != http.StatusConflict {
			t.Fatalf("%s returned %d (%s)", path, response.Code, response.Body.String())
		}
	}
}

func TestRawReplayVerifiesURIDateRevisitDonor(t *testing.T) {
	const uri = "https://example.org/"
	const donorURI = "https://example.org/donor/"
	const payload = "body"
	const digest = "sha1:AIED6RLZ4CFGCJBFYDA2C7XEPLOXQO4U"
	donorDate := time.Date(2026, 1, 1, 12, 0, 0, 123000000, time.UTC)
	revisitDate := donorDate.Add(time.Hour)
	warc := func(kind string, target string, date time.Time, id string, headers string, content string) string {
		return fmt.Sprintf("WARC/1.1\r\nWARC-Type: %s\r\nWARC-Record-ID: <%s>\r\nWARC-Date: %s\r\nWARC-Target-URI: %s\r\n%sContent-Type: application/http; msgtype=response\r\nContent-Length: %d\r\n\r\n%s\r\n\r\n", kind, id, date.Format(time.RFC3339Nano), target, headers, len(content), content)
	}
	const donorID = "urn:uuid:20add669-b781-4c66-977c-e50e280a69e9"
	const revisitID = "urn:uuid:20add669-b781-4c66-977c-e50e280a69ea"
	for _, tc := range []struct {
		name      string
		date      time.Time
		digest    string
		want      int
		duplicate string
	}{
		{"POST donor", donorDate, digest, http.StatusOK, ""},
		{"exact donor", donorDate, digest, http.StatusOK, ""},
		{"identical donors", donorDate, digest, http.StatusOK, "donor"},
		{"conflicting donors", donorDate, digest, http.StatusInternalServerError, "other"},
		{"missing donor", donorDate.Add(time.Second), digest, http.StatusNotFound, ""},
		{"different payload", donorDate, "sha1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", http.StatusInternalServerError, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			source := filepath.Join(dir, "captures.warc")
			full := warc("response", donorURI, donorDate, donorID, "", "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nX-Record: donor\r\n\r\n"+payload)
			headers := "WARC-Profile: http://netpreserve.org/warc/1.1/revisit/identical-payload-digest\r\nWARC-Refers-To-Target-URI: " + donorURI + "\r\nWARC-Refers-To-Date: " + tc.date.Format(time.RFC3339Nano) + "\r\nWARC-Payload-Digest: " + tc.digest + "\r\n"
			revisit := warc("revisit", uri, revisitDate, revisitID, headers, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nX-Record: revisit\r\n\r\n")
			if err := os.WriteFile(source, []byte(full+revisit), 0600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			db, err := cdxstore.Open(ctx, cdxstore.Options{Format: badgerFormat, Dir: filepath.Join(dir, "index")})
			if err != nil {
				t.Fatal(err)
			}
			defer testcheck.Close(t, db)
			generation, err := db.BeginFile(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range []*cdx.Capture{
				{Uri: donorURI, Sts: timestamppb.New(donorDate.Truncate(time.Second)), Rid: donorID, Srt: "response"},
				{Uri: uri, Sts: timestamppb.New(revisitDate), Rid: revisitID, Srt: "revisit", Offset: int64(len(full))},
			} {
				c.Ssu, c.Source, c.Filename = cdx.CanonicalKey(c.GetUri()), source, info.Name()
				// Mimic an embedded CDX row retaining the original trailing slash.
				if c.Uri == donorURI && tc.name == "POST donor" {
					cdx.Augment(c, "POST", "a=1")
				} else if c.Uri == donorURI {
					c.Ssu += "/"
				}
				c.Ref = fmt.Sprintf("warcfile:%s#%d", source, c.GetOffset())
				if err := db.WriteCapture(generation, c); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.FinishFile(generation, source, info.Size(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if tc.duplicate != "" {
				copyPath := filepath.Join(dir, "second.warc")
				copyRecord := strings.Replace(full, "X-Record: donor", "X-Record: "+tc.duplicate, 1)
				if err := os.WriteFile(copyPath, []byte(copyRecord), 0600); err != nil {
					t.Fatal(err)
				}
				ingest := &indexcmd.IndexOptions{Paths: []string{copyPath}, Sink: db, Concurrency: 1, FileWalker: filewalker.New(filewalker.WithFs(afero.NewOsFs()))}
				defer testcheck.Close(t, ingest)
				if err := ingest.Run(ctx); err != nil {
					t.Fatal(err)
				}
			}
			handler := (&ServeOptions{allowSameOriginReplay: true}).databaseHandler(ctx, db)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/warcserver/web/20260101130000id_/"+uri, nil))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusOK && (strings.TrimSpace(w.Body.String()) != payload || w.Header().Get("X-Record") != "revisit") {
				t.Fatalf("unexpected reconstructed capture: headers=%v body=%q", w.Header(), w.Body.String())
			}
		})
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
	ids := jsontext.NewDecoder(strings.NewReader(get("/id")))
	for ids.PeekKind() != 0 {
		var row struct {
			ID       string `json:"id"`
			Filename string `json:"filename"`
			Offset   int    `json:"offset"`
		}
		if err := json.UnmarshalDecode(ids, &row); err != nil {
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

func TestWACZStoredKeyReplayAndMutation(t *testing.T) {
	ctx := t.Context()
	db, err := cdxstore.Open(ctx, cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	const original = "https://example.org/page/"
	const block = "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"
	warc := fmt.Sprintf("WARC/1.1\r\nWARC-Type: response\r\nWARC-Record-ID: <urn:uuid:20add669-b781-4c66-977c-e50e280a69e9>\r\nWARC-Date: 2025-01-01T00:00:00Z\r\nWARC-Target-URI: %s\r\nContent-Type: application/http; msgtype=response\r\nContent-Length: %d\r\n\r\n%s\r\n\r\n", original, len(block), block)
	row := fmt.Sprintf("org,example)/page/ 20250101000000 {\"url\":%q,\"filename\":\"a.warc\",\"offset\":0,\"length\":%d,\"orig.filename\":\"original\",\"orig.offset\":\"0\",\"orig.length\":\"5\"}\n", original, len(warc))
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for k, v := range map[string]string{"archive/a.warc": warc, "indexes/index.cdxj": row,
		"archive/sub/b.warc": "donor", "datapackage.json": `{"resources":[{"name":"original","path":"archive/sub/b.warc"}]}`} {
		w, e := z.Create(k)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = io.WriteString(w, v); e != nil {
			t.Fatal(e)
		}
	}
	if e := z.Close(); e != nil {
		t.Fatal(e)
	}
	name := filepath.Join(t.TempDir(), "a.wacz")
	if e := os.WriteFile(name, buf.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	o := indexcmd.IndexOptions{Sink: db, Paths: []string{name}, Concurrency: 1, FileWalker: filewalker.New(filewalker.WithFs(afero.NewOsFs()))}
	if e := o.Run(ctx); e != nil {
		t.Fatal(e)
	}
	defer testcheck.Close(t, &o)
	opts := ServeOptions{allowSameOriginReplay: true}
	h := opts.databaseHandler(ctx, db)
	call := func(method, path, body string) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body)))
		if w.Code < 200 || w.Code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	if got := call("GET", "/warcserver/web/20250101000000id_/"+original, ""); got != "hello" {
		t.Fatalf("raw replay: %q", got)
	}
	reader, err := wacz.Open(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, reader)
	for _, handler := range []http.Handler{h, opts.directHandler(reader)} {
		index := httptest.NewRecorder()
		handler.ServeHTTP(index, httptest.NewRequestWithContext(ctx, "GET", "/archives/cdx?output=json&url="+original, nil))
		var fields map[string]any
		if err := json.Unmarshal(index.Body.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		filename, ok := fields["orig.filename"].(string)
		if !ok || fields["orig.offset"] != "0" || fields["orig.length"] != "5" {
			t.Fatalf("donor location: %v", fields)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(ctx, "GET", "/archives/"+filename, nil))
		if response.Code != http.StatusOK || response.Body.String() != "donor" {
			t.Fatalf("donor GET: %d %q", response.Code, response.Body.String())
		}
	}
	export := call("GET", "/export", "")
	if !strings.HasPrefix(export, "org,example)/page/ ") {
		t.Fatalf("initial export: %q", export)
	}
	call("POST", "/cdx", export)
	if got := strings.Fields(call("GET", "/export", "")); len(got) == 0 || got[0] != "org,example)/page" {
		t.Fatalf("upserted capture: %q", got)
	}
	if got := strings.TrimSpace(call("GET", "/export", "")); strings.Contains(got, "\n") {
		t.Fatalf("duplicate captures: %q", got)
	}
	call("POST", "/delete", export)
	if got := call("GET", "/export", ""); got != "" {
		t.Fatalf("after deletion: %q", got)
	}
	reindex := indexcmd.IndexOptions{Sink: db, Paths: []string{name}, Concurrency: 1, FileWalker: filewalker.New(filewalker.WithFs(afero.NewOsFs()))}
	defer testcheck.Close(t, &reindex)
	if err := reindex.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := call("GET", "/export", ""); got != export {
		t.Fatalf("reindex restored %q, want %q", got, export)
	}
}

func TestFreshFulltextDatabaseMode(t *testing.T) {
	opts := ServeOptions{fulltext: true, address: "invalid-address", database: cdxstore.Options{Format: badgerFormat, Dir: t.TempDir()}}
	if err := opts.runNative(t.Context()); err == nil || !strings.Contains(err.Error(), "missing port") {
		t.Fatalf("expected listener setup after opening fulltext index, got %v", err)
	}
	testcheck.Close(t, &opts)
	db, err := cdxstore.Open(t.Context(), opts.database)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	if db.IndexOnly() {
		t.Fatal("fresh fulltext database is index-only")
	}
}

func TestSkipOverflowScansRoutes(t *testing.T) {
	ctx := t.Context()
	db, err := cdxstore.Open(ctx, cdxstore.Options{Format: badgerFormat, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	source := "sample.warc"
	g, err := db.BeginFile(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	long := "https://example.org/" + strings.Repeat("x", 70000)
	for i, raw := range []string{"https://example.org/a", long} {
		c := &cdx.Capture{Uri: raw, Ssu: cdx.CanonicalKey(raw), Source: source, Filename: source, Ref: fmt.Sprintf("warcfile:%s#%d", source, i), Offset: int64(i), Sts: timestamppb.Now()}
		if err := db.WriteCapture(g, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.FinishFile(g, source, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	o := ServeOptions{fulltext: true}
	o.httpOptions.SkipOverflowScans = true
	h := o.databaseHandler(ctx, db)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(ctx, "GET", path, nil))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		return w
	}
	for _, route := range []string{"/cdx", "/search/cdx", "/archives/cdx"} {
		for _, q := range []string{"url=example.org&matchType=host", "url=example.org&matchType=domain", "url=https://example.org/*", "url=*.example.org"} {
			w := get(route + "?" + q)
			if w.Header().Get("X-CDX-Overflow-Skipped") != "true" || strings.Contains(w.Body.String(), long) || !strings.Contains(w.Body.String(), "example.org/a") {
				t.Fatalf("%s %s: incorrect broad results", route, q)
			}
			if route == "/search/cdx" && !strings.Contains(w.Body.String(), `"overflowSkipped":true`) {
				t.Fatal("missing console status")
			}
		}
		for _, order := range []string{"", "&sort=reverse", "&sort=closest&closest=2020"} {
			w := get(route + "?url=" + long + order)
			if w.Header().Get("X-CDX-Overflow-Skipped") != "" || !strings.Contains(w.Body.String(), long) {
				t.Fatalf("%s %s: missing exact capture", route, order)
			}
		}
	}
	if !strings.Contains(get("/export").Body.String(), long) {
		t.Fatal("missing overflow capture in export")
	}
	row := `ignored 20260101000000 {"url":"https://new.example.org/path","filename":"f","offset":0,"length":1,"mime":"original"}`
	upload := func(line string, remove bool) {
		t.Helper()
		if _, err := db.Upload(ctx, strings.NewReader(line), cdxstore.UploadOptions{Delete: remove}); err != nil {
			t.Fatal(err)
		}
	}
	upload(row, false)
	upload(strings.Replace(row, "original", "replacement", 1), false)
	broad := "/cdx?url=*.example.org"
	if w := get(broad + "&limit=1"); w.Result().Trailer.Get("X-Cdx-Truncated") != "true" {
		t.Fatal("missing result limit signal")
	}
	if body := get(broad).Body.String(); !strings.Contains(body, "replacement") || strings.Contains(body, "original") {
		t.Fatal("incorrect replacement visibility")
	}
	upload(row, true)
	if strings.Contains(get(broad).Body.String(), "new.example.org/path") {
		t.Fatal("deleted capture still visible")
	}

}
