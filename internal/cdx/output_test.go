package index

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bufio"

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
	c := &Capture{Ssu: "com,example)/", Uri: "https://example.com/", Sts: timestamppb.New(time.Now()), Filename: "a.warc", Rle: 7}
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
