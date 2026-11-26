// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestExternalSort(t *testing.T) {
	dir := t.TempDir()
	s := NewSorter(dir, true)
	for i := range 70 {
		if e := s.Add(t.Context(), []string{"b", "a", "a"}[i%3]); e != nil {
			t.Fatal(e)
		}
		if e := s.spill(); e != nil {
			t.Fatal(e)
		}
	}
	var out bytes.Buffer
	if e := s.WriteTo(t.Context(), &out); e != nil {
		t.Fatal(e)
	}
	if out.String() != "a\nb\n" {
		t.Fatal(out.String())
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	files, e := os.ReadDir(dir)
	if e != nil || len(files) != 0 {
		t.Fatalf("temporary files remain: %v, %v", files, e)
	}
}

func TestOutputPublicationAndProtobuf(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "index.cdxj")
	if e := os.WriteFile(target, []byte("old"), 0600); e != nil {
		t.Fatal(e)
	}
	c := &Capture{
		Ssu:      "com,example)/",
		Uri:      "https://example.com/",
		Sts:      timestamppb.New(time.Now()),
		Filename: "a.warc",
		Rle:      7,
	}
	s, e := NewOutput(io.Discard, target, "cdxj", dir, false)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.WriteCapture(t.Context(), c); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if e = s.Flush(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(target)
	if e != nil || string(b) != "old" {
		t.Fatalf("failed output replaced target: %s %v", b, e)
	}
	var out bytes.Buffer
	p, e := NewOutput(&out, "-", "cdxpb", dir, false)
	if e != nil {
		t.Fatal(e)
	}
	for range 2 {
		if e = p.WriteCapture(t.Context(), c); e != nil {
			t.Fatal(e)
		}
	}
	if e = p.Flush(t.Context()); e != nil {
		t.Fatal(e)
	}
	if e = p.Close(); e != nil {
		t.Fatal(e)
	}
	r := bufio.NewReader(&out)
	for range 2 {
		got := &Capture{}
		if e = protodelim.UnmarshalFrom(r, got); e != nil {
			t.Fatal(e)
		}
		if got.GetUri() != c.GetUri() {
			t.Fatal(got)
		}
	}
	if e = protodelim.UnmarshalFrom(r, &Capture{}); !errors.Is(e, io.EOF) {
		t.Fatal(e)
	}
}

func TestQueryValidation(t *testing.T) {
	for _, q := range []map[string][]string{{"url": {"example.com"}, "page": {"-1"}}, {"url": {"example.com"}, "filter": {"~url:["}}, {"url": {"example.com"}, "sort": {"closest"}}, {"url": {"example.com"}, "from": {"not-a-date"}}} {
		if _, e := ParseQuery(q); e == nil {
			t.Fatalf("accepted invalid query %v", q)
		}
	}
	q, e := ParseQuery(map[string][]string{"url": {"example.com"}, "matchType": {"domain"}})
	if e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"com,example)/", "com,example,sub)/"} {
		if !q.Accept(&Capture{Ssu: key, Sts: timestamppb.Now()}) {
			t.Fatal(key)
		}
	}
	if q.Accept(&Capture{Ssu: "com,examplex)/", Sts: timestamppb.Now()}) {
		t.Fatal("domain boundary crossed")
	}
}

type scanFunc func(context.Context, string, string, func(*Capture) error) error

func (f scanFunc) Scan(ctx context.Context, lo, hi string, emit func(*Capture) error) error {
	return f(ctx, lo, hi, emit)
}

func TestExactLookupKeySelection(t *testing.T) {
	const native = "com,example)/page"
	for _, tc := range []struct {
		raw, params, preferred string
	}{
		{"https://example.com/page/", "", native + "/"},
		{"https://example.com/page/#fragment", "", native + "/"},
		{"http://example.com/page", "", ""},
		{"https://example.com/", "", ""},
		{"https://example.com/page%2F", "", ""},
		{"https://example.com/page/?", "", ""},
		{"https://example.com/page/?x=/", "", ""},
		{"https://example.com/page/", "urlkey=com,example)/other", ""},
		{"https://example.com/page/", "method=POST", ""},
		{"https://example.com/page/", "method=POST&requestBody=value", ""},
		{"https://example.com/page/*", "", ""},
		{"https://example.com/page/", "matchType=host", ""},
		{"https://example.com/page/", "page=0", ""},
		{"ftp://example.com/page/", "", ""},
	} {
		v, err := url.ParseQuery(tc.params)
		if err != nil {
			t.Fatal(err)
		}
		v.Set("url", tc.raw)
		q, err := ParseQuery(v)
		if err != nil || q.preferredExactKey != tc.preferred {
			t.Errorf("%s: preferred key %q, want %q; err=%v", v.Encode(), q.preferredExactKey, tc.preferred, err)
		}
	}
}

func TestExactSlashLookup(t *testing.T) {
	const native = "com,example)/page"
	const slash = native + "/"
	scanErr := errors.New("scan failed")
	for _, tc := range []struct {
		name                      string
		query                     string
		preferredRows, nativeRows int
		scanErr                   error
		cancel                    bool
		wantErr                   error
		wantKeys                  []string
		wantStatus                int32
		truncated                 bool
	}{
		{name: "both", preferredRows: 1, nativeRows: 1, wantKeys: []string{slash}, wantStatus: 308},
		{name: "slash only", preferredRows: 1, wantKeys: []string{slash}, wantStatus: 308},
		{name: "native only", nativeRows: 1, wantKeys: []string{slash, native}, wantStatus: 200},
		{name: "absent", wantKeys: []string{slash, native}},
		{name: "status filter", query: "filter==status:200", preferredRows: 1, nativeRows: 1, wantKeys: []string{slash}},
		{name: "date filter", query: "from=2030", preferredRows: 1, nativeRows: 1, wantKeys: []string{slash}},
		{name: "truncated", query: "filter==status:200", preferredRows: 2, nativeRows: 1, wantKeys: []string{slash}, truncated: true},
		{name: "error", scanErr: scanErr, wantErr: scanErr, wantKeys: []string{slash}},
		{name: "cancel", cancel: true, wantErr: context.Canceled, wantKeys: []string{slash}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			v, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			v.Set("url", "https://example.com/page/")
			q, err := ParseQuery(v)
			if err != nil {
				t.Fatal(err)
			}
			q.ScanLimit, q.State = 1, &QueryState{}
			var keys []string
			src := scanFunc(func(_ context.Context, lo, hi string, emit func(*Capture) error) error {
				key := strings.TrimSuffix(lo, " ")
				keys = append(keys, key)
				if lo != key+" " || hi != prefixEnd(lo) {
					t.Fatalf("unexpected bounds %q %q", lo, hi)
				}
				rows, status := tc.nativeRows, int32(200)
				if key == slash {
					rows, status = tc.preferredRows, 308
				}
				if tc.cancel {
					cancel()
				}
				for range rows {
					if err := emit(&Capture{Ssu: key, Hsc: status, Sts: timestamppb.New(time.Unix(0, 0))}); err != nil {
						return err
					}
				}
				return tc.scanErr
			})
			var status int32
			_, err = q.Run(ctx, src, func(c *Capture) error { status = c.GetHsc(); return nil })
			if !errors.Is(err, tc.wantErr) || !slices.Equal(keys, tc.wantKeys) || status != tc.wantStatus || q.State.Truncated != tc.truncated {
				t.Fatalf("keys=%v status=%d state=%+v err=%v", keys, status, q.State, err)
			}
		})
	}
}

func TestStorageRefLiteralHash(t *testing.T) {
	name, off, e := ParseStorageRef("warcfile:/archive/a#b.warc#0010")
	if e != nil || !strings.Contains(name, "#b") || off != 10 {
		t.Fatalf("%s %d %v", name, off, e)
	}
}

func TestJSONPropertiesPreserveNumbers(t *testing.T) {
	line := []byte(`com,example)/ 20200101000000 {"url":"https://example.com/","large":9007199254740993,"unknown":{"array":[1,null,true]}}`)
	c, e := ParseLine(line)
	if e != nil {
		t.Fatal(e)
	}
	b, e := Line(c)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(b, []byte(`"large":9007199254740993`)) || !bytes.Contains(b, []byte(`"array":[1,null,true]`)) {
		t.Fatalf("properties changed: %s", b)
	}
}

func TestConsoleCDXCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, query                    string
		scan, limit, scanned, returned int
		truncated, fail                bool
	}{
		{"empty", "&from=2050", 10, 10, 2, 0, false, false},
		{"scan limit", "&from=1971", 1, 10, 1, 0, true, false},
		{"result limit", "", 10, 1, 2, 1, true, false},
		{"failed after row", "", 10, 10, 1, 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := scanFunc(func(_ context.Context, _, _ string, emit func(*Capture) error) error {
				for _, year := range []int{1970, 1971} {
					if err := emit(&Capture{Uri: "https://example.com/", Sts: timestamppb.New(time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC))}); err != nil {
						return err
					}
					if tc.fail {
						return errors.New("read failed")
					}
				}
				return nil
			})
			w := httptest.NewRecorder()
			HTTPQueryOptions{ScanLimit: tc.scan, ResultLimit: tc.limit, Console: true}.Handler(src)(w, httptest.NewRequest("GET", "/search/cdx?url=example.com"+tc.query, nil))
			rows := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
			var status []struct {
				Scanned, Returned int
				Truncated         bool
				Error             string
			}
			if err := json.Unmarshal([]byte(rows[len(rows)-1]), &status, json.MatchCaseInsensitiveNames(true)); err != nil || len(status) != 1 {
				t.Fatal(w.Body.String(), err)
			}
			got := status[0]
			if len(rows) != 1+tc.returned || got.Scanned != tc.scanned || got.Returned != tc.returned || got.Truncated != tc.truncated || (got.Error != "") != tc.fail {
				t.Fatal(w.Body.String())
			}
		})
	}
}
