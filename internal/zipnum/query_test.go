package zipnum

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
)

func TestQueryReader(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for _, host := range []string{"com,example)", "com,example,www)", "org,other)"} {
		for n := range 25 {
			lines = append(lines, fmt.Sprintf(`%s/ 202001%02d000000 {"url":"http://example.com/","status":"200","custom":[1,"x"],"filename":"a.warc","offset":"0","length":"1"}`, host, n%20+1))
		}
	}
	sort.Strings(lines)
	raw := filepath.Join(dir, "input.cdxj")
	if e := os.WriteFile(raw, []byte(strings.Join(lines, "\n")+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(dir, "cluster")
	if e := (Options{Inputs: []string{raw}, Output: root, Concurrency: 2, LinesPerBlock: 2, BlocksPerPart: 2}).Run(t.Context()); e != nil {
		t.Fatal(e)
	}
	r, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	queries := []map[string][]string{{"url": {"example.com"}}, {"url": {"example.com"}, "matchType": {"domain"}}, {"url": {"example.com"}, "from": {"20200105"}, "to": {"20200108"}}, {"url": {"example.com"}, "sort": {"reverse"}, "limit": {"3"}}, {"url": {"example.com"}, "sort": {"closest"}, "closest": {"20200107000000"}, "limit": {"4"}}, {"url": {"absent.net"}}}
	for _, v := range queries {
		q, e := index.ParseQuery(v)
		if e != nil {
			t.Fatal(e)
		}
		collect := func(src index.QuerySource) []string {
			var got []string
			_, e := q.Run(t.Context(), src, func(c *index.Capture) error {
				b, e := index.Line(c)
				if e != nil {
					return e
				}
				got = append(got, string(b))
				return nil
			})
			if e != nil {
				t.Fatal(e)
			}
			return got
		}
		got, want := collect(r), collect(referenceLines(lines))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("query %v: got %d want %d", v, len(got), len(want))
		}
	}
	q, _ := index.ParseQuery(map[string][]string{"url": {"example.com"}, "page": {"0"}, "pageSize": {"2"}})
	var pagesResults []string
	pages, e := q.Run(t.Context(), r, func(c *index.Capture) error {
		b, _ := index.Line(c)
		pagesResults = append(pagesResults, string(b))
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	for page := 1; page < pages; page++ {
		q.Page = page
		if _, e = q.Run(t.Context(), r, func(c *index.Capture) error {
			b, _ := index.Line(c)
			pagesResults = append(pagesResults, string(b))
			return nil
		}); e != nil {
			t.Fatal(e)
		}
	}
	if len(pagesResults) != 25 {
		t.Fatalf("pagination lost duplicates: %d", len(pagesResults))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if e = r.Scan(ctx, "", "", func(*index.Capture) error { return nil }); e == nil {
		t.Fatal("cancellation ignored")
	}
	// Corrupt a noncandidate part: a narrow lookup must never decompress it.
	last, e := r.block(len(r.offsets) - 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, partDir, last.file), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	q, _ = index.ParseQuery(map[string][]string{"url": {"example.com"}})
	if _, e = q.Run(t.Context(), r, func(*index.Capture) error { return nil }); e != nil {
		t.Fatalf("narrow lookup touched unrelated part: %v", e)
	}
}

type referenceLines []string

func (r referenceLines) Scan(ctx context.Context, lo, hi string, emit func(*index.Capture) error) error {
	for _, line := range r {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if line < lo || (hi != "" && line >= hi) {
			continue
		}
		c, e := index.ParseLine([]byte(line))
		if e != nil {
			return e
		}
		if e = emit(c); e != nil {
			return e
		}
	}
	return nil
}

func TestQueryRejectsCorruptSummary(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "input.cdxj")
	line := `com,example)/ 20200101000000 {"url":"https://example.com/"}` + "\n"
	if e := os.WriteFile(raw, []byte(line), 0600); e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(dir, "cluster")
	if e := (Options{Inputs: []string{raw}, Output: root, Concurrency: 1}).Run(t.Context()); e != nil {
		t.Fatal(e)
	}
	summary := filepath.Join(root, indexFile)
	b, e := os.ReadFile(summary)
	if e != nil {
		t.Fatal(e)
	}
	fields := strings.Split(strings.TrimSpace(string(b)), "\t")
	for _, tc := range []struct {
		name   string
		column int
		value  string
	}{{"unsafe filename", 1, "../escape.gz"}, {"sequence", 4, "2"}, {"bad timestamp", 0, "com,example)/ invalid"}, {"negative offset", 2, "-1"}} {
		t.Run(tc.name, func(t *testing.T) {
			row := append([]string(nil), fields...)
			row[tc.column] = tc.value
			if e := os.WriteFile(summary, []byte(strings.Join(row, "\t")+"\n"), 0600); e != nil {
				t.Fatal(e)
			}
			r, e := Open(root)
			if e == nil {
				_ = r.Close()
				t.Fatal("corrupt summary accepted")
			}
		})
	}
}

func TestQueryLongRowsAndSummaryKeys(t *testing.T) {
	dir := t.TempDir()
	key := "com,example)/" + strings.Repeat("a", 1<<20)
	neighbor := key + "b"
	raw := filepath.Join(dir, "input.cdxj")
	data := key + " 20200101000000 " + `{"url":"https://example.com/","metadata":"` + strings.Repeat("x", 16<<20) + `"}` + "\n" + neighbor + " 20200101000000 " + `{"url":"https://example.com/b"}` + "\n"
	if err := os.WriteFile(raw, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "cluster")
	if err := (Options{Inputs: []string{raw}, Output: root, Concurrency: 1, LinesPerBlock: 2, BlocksPerPart: 1}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	reader, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, k := range []string{key, neighbor} {
		count := 0
		err = reader.Scan(t.Context(), k+" ", k+"!", func(*index.Capture) error { count++; return nil })
		if count != 1 || err != nil {
			t.Fatalf("matches=%d error=%v", count, err)
		}
	}
}
