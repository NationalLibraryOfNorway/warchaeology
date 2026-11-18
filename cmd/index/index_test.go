package index

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"

	"github.com/nationallibraryofnorway/gowarc/v3"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filter"
	"github.com/spf13/afero"
)

type fixture struct {
	Name string `json:"name"`
	Post bool   `json:"post"`
	Warc []byte `json:"warc"`
	Cdxj string `json:"cdxj"`
}

func fixtures(t *testing.T) []fixture {
	t.Helper()
	b, e := os.ReadFile("testdata/reference.json")
	if e != nil {
		t.Fatal(e)
	}
	var f []fixture
	if e = json.Unmarshal(b, &f); e != nil {
		t.Fatal(e)
	}
	return f
}

func options(sink cdx.Sink, paths ...string) *IndexOptions {
	return &IndexOptions{Paths: paths, Concurrency: 2, FileWalker: filewalker.New(filewalker.WithFs(afero.NewOsFs())), Filter: filter.New(filter.WithRecordTypes(gowarc.Response | gowarc.Revisit | gowarc.Resource | gowarc.Metadata)), DefaultRecords: true, Sink: sink, WarcRecordOptions: []gowarc.WarcRecordOption{gowarc.WithSyntaxErrorPolicy(gowarc.ErrWarn), gowarc.WithSpecViolationPolicy(gowarc.ErrWarn)}}
}

func entries(t *testing.T, s string) []any {
	t.Helper()
	out := []any{}
	for line := range strings.SplitSeq(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			t.Fatalf("invalid line %q", line)
		}
		var m any
		if e := json.Unmarshal([]byte(fields[2]), &m); e != nil {
			t.Fatal(e)
		}
		out = append(out, []any{fields[0], fields[1], m})
	}
	return out
}

func TestPinnedCDXJ(t *testing.T) {
	for _, f := range fixtures(t) {
		name := f.Name
		if f.Post {
			name += "-post"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), f.Name)
			if e := os.WriteFile(path, f.Warc, 0600); e != nil {
				t.Fatal(e)
			}
			var out bytes.Buffer
			sink, e := cdx.NewOutput(&out, "-", "cdxj", "", false)
			if e != nil {
				t.Fatal(e)
			}
			defer sink.Close()
			o := options(sink, path)
			o.PostAppend = f.Post
			defer o.Close()
			if e = o.Run(context.Background()); e != nil {
				t.Fatal(e)
			}
			got, want := entries(t, out.String()), entries(t, f.Cdxj)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got:\n%s\nwant:\n%s", out.String(), f.Cdxj)
			}
		})
	}
}

func TestPinnedPostQueries(t *testing.T) {
	b, e := os.ReadFile("testdata/post.json")
	if e != nil {
		t.Fatal(e)
	}
	var cases []struct {
		Name   string `json:"name"`
		Mime   string `json:"mime"`
		Body   string `json:"body"`
		Length string `json:"length"`
		Query  string `json:"query"`
	}
	if e = json.Unmarshal(b, &cases); e != nil {
		t.Fatal(e)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			b, e := base64.StdEncoding.DecodeString(c.Body)
			if e != nil {
				t.Fatal(e)
			}
			got, e := cdx.RequestQuery(c.Mime, c.Length, bytes.NewReader(b))
			if e != nil {
				t.Fatal(e)
			}
			if got != c.Query {
				t.Fatalf("got %q want %q", got, c.Query)
			}
		})
	}
}

type failingSink struct {
	err   error
	flush bool
}

func (s failingSink) WriteCapture(context.Context, *cdx.Capture) error {
	if s.flush {
		return nil
	}
	return s.err
}

func (s failingSink) Flush(context.Context) error { return s.err }

func (s failingSink) Close() error { return nil }

func TestIngestionErrors(t *testing.T) {
	sentinel := errors.New("sink failure")
	for _, flush := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "flush"}[flush], func(t *testing.T) {
			f := fixtures(t)[0]
			p := filepath.Join(t.TempDir(), f.Name)
			if e := os.WriteFile(p, f.Warc, 0600); e != nil {
				t.Fatal(e)
			}
			o := options(failingSink{sentinel, flush}, p)
			defer o.Close()
			if e := o.Run(context.Background()); !errors.Is(e, sentinel) {
				t.Fatalf("got %v", e)
			}
		})
	}
}

func TestArchiveSourceRetrieval(t *testing.T) {
	f := fixtures(t)[0]
	archive := filepath.Join(t.TempDir(), "archive#!.zip")
	file, e := os.Create(archive)
	if e != nil {
		t.Fatal(e)
	}
	zw := zip.NewWriter(file)
	w, e := zw.Create("inside/a#b.warc")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = w.Write(f.Warc); e != nil {
		t.Fatal(e)
	}
	if e = errors.Join(zw.Close(), file.Close()); e != nil {
		t.Fatal(e)
	}
	db, e := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	o := options(db, archive)
	o.FileWalker.Recursive = true
	if e = o.Run(t.Context()); e != nil {
		t.Fatal(e)
	}
	if e = o.Close(); e != nil {
		t.Fatal(e)
	}
	var ref string
	if e = db.Scan(t.Context(), "com,example)/ ", "com,example)/!", func(c *cdx.Capture) error {
		if c.GetSrt() == "response" {
			ref = c.GetRef()
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if ref == "" {
		t.Fatal("no indexed record")
	}
	l := loader.FileStorageLoader{}
	rec, e := l.RecordByStorageRef(t.Context(), ref)
	if e != nil {
		t.Fatal(e)
	}
	defer rec.Close()
	r, e := rec.Block().(gowarc.PayloadBlock).PayloadBytes()
	if e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(r)
	if e != nil || !bytes.Contains(body, []byte("Hello replay")) {
		t.Fatalf("retrieval: %s %v", body, e)
	}
}

func TestPinnedSURTCompatibility(t *testing.T) {
	b, e := os.ReadFile("testdata/surt.json")
	if e != nil {
		t.Fatal(e)
	}
	var cases []struct {
		URL       string `json:"url"`
		Reference string `json:"reference"`
	}
	if e = json.Unmarshal(b, &cases); e != nil {
		t.Fatal(e)
	}
	differences := map[string]string{"http://example.com\\path": "com,example)/path", "dns:Example.COM": "dns:Example.COM"}
	for _, c := range cases {
		want := c.Reference
		if intentional, ok := differences[c.URL]; ok {
			want = intentional
		}
		if got := cdx.CanonicalKey(c.URL); got != want {
			t.Errorf("%q: got %q want %q", c.URL, got, want)
		}
	}
}

func TestInflatedMemberReusesPrefix(t *testing.T) {
	for _, n := range []int{500, 1000} {
		data := bytes.Repeat([]byte("abcdefg"), n)
		source := bytes.NewReader(data)
		cache, err := os.CreateTemp(t.TempDir(), "cache")
		if err != nil {
			t.Fatal(err)
		}
		defer cache.Close()
		m := inflatedMember{source: source, ctx: t.Context(), cache: cache}
		var b [7]byte
		for i := 0; i < n; i++ {
			for _, offset := range []int64{int64(i * 7), 0, int64(i * 7)} {
				if _, err := m.ReadAt(b[:], offset); err != nil || string(b[:]) != "abcdefg" {
					t.Fatalf("read at %d: %q %v", offset, b, err)
				}
			}
			if consumed := len(data) - source.Len(); consumed != (i+1)*7 {
				t.Fatalf("inflated %d bytes after %d records", consumed, i+1)
			}
		}
	}
}
