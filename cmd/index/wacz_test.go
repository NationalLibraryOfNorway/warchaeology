// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	archivefs "github.com/nationallibraryofnorway/warchaeology/v5/internal/fs"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"github.com/spf13/afero"
)

func TestWACZImportAndRetrieve(t *testing.T) {
	fixture := fixtures(t)[0]
	name := filepath.Join(t.TempDir(), "sample.wacz")
	f, e := os.Create(name)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	entry, e := z.CreateHeader(&zip.FileHeader{Name: "archive/" + fixture.Name, Method: zip.Store})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = entry.Write(fixture.Warc); e != nil {
		t.Fatal(e)
	}
	// Use a native export so offsets and filenames match this exact member.
	line := fixture.Cdxj
	entry, e = z.Create("indexes/index.cdxj")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = entry.Write([]byte(line)); e != nil {
		t.Fatal(e)
	}
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	db, e := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	defer testcheck.Close(t, db)
	o := options(db, name)
	defer testcheck.Close(t, o)
	if e = o.Run(context.Background()); e != nil {
		t.Fatal(e)
	}
	n := 0
	e = db.Scan(t.Context(), "", "", func(c *cdx.Capture) error {
		n++
		var src map[string]any
		if e := json.Unmarshal([]byte(c.GetSource()), &src); e != nil {
			return e
		}
		rec, e := (loader.FileStorageLoader{}).RecordByStorageRef(t.Context(), c.GetRef())
		if e != nil {
			return e
		}
		return rec.Close()
	})
	if e != nil || n == 0 {
		t.Fatalf("captures=%d error=%v", n, e)
	}
	o2 := options(db, name)
	defer testcheck.Close(t, o2)
	if e = o2.Run(t.Context()); e != nil {
		t.Fatal(e)
	}
	count := captureCount(t, db)
	if count != n {
		t.Fatalf("repeat changed count: %d != %d", count, n)
	}
}

func TestZIPMemberPathAliases(t *testing.T) {
	f := fixtures(t)[0]
	member := "archive/" + f.Name
	name := filepath.Join(t.TempDir(), "aliases.zip")
	if err := os.WriteFile(name, packageBytes(t, map[string][]byte{member: f.Warc}), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: badgerFormat, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	var want int
	for _, spelling := range []string{"/" + member, member, "./" + member} {
		fs, err := archivefs.ResolveFilesystem(afero.NewOsFs(), name)
		if err != nil {
			t.Fatal(err)
		}
		o := options(db, spelling)
		o.FileWalker = filewalker.New(filewalker.WithFs(fs))
		o.FileWalker.SourceRoot = name
		if err := errors.Join(o.Run(t.Context()), o.Close()); err != nil {
			t.Fatal(err)
		}
		count := captureCount(t, db)
		if count == 0 || (want != 0 && count != want) {
			t.Fatalf("indexing %q produced %d captures, previous count %d", spelling, count, want)
		}
		want = count
	}
}

func TestWACZIndexModes(t *testing.T) {
	for _, mode := range []string{"missing", "gzip", "broken", "native"} {
		t.Run(mode, func(t *testing.T) {
			fixture := fixtures(t)[0]
			name := filepath.Join(t.TempDir(), "test.wacz")
			f, e := os.Create(name)
			if e != nil {
				t.Fatal(e)
			}
			z := zip.NewWriter(f)
			w, e := z.CreateHeader(&zip.FileHeader{Name: "archive/" + fixture.Name, Method: zip.Store})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = w.Write(fixture.Warc); e != nil {
				t.Fatal(e)
			}
			if mode != "missing" {
				n := "indexes/index.cdxj"
				content := []byte(fixture.Cdxj)
				if mode == "broken" || mode == "native" {
					content = []byte("invalid\n")
				}
				if mode == "gzip" {
					n += ".gz"
					var b bytes.Buffer
					g := gzip.NewWriter(&b)
					if _, e = g.Write(content); e != nil {
						t.Fatal(e)
					}
					if e = g.Close(); e != nil {
						t.Fatal(e)
					}
					content = b.Bytes()
				}
				w, e = z.Create(n)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = w.Write(content); e != nil {
					t.Fatal(e)
				}
			}
			if e = z.Close(); e != nil {
				t.Fatal(e)
			}
			if e = f.Close(); e != nil {
				t.Fatal(e)
			}
			db, e := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: t.TempDir()})
			if e != nil {
				t.Fatal(e)
			}
			defer testcheck.Close(t, db)
			o := options(db, name)
			defer testcheck.Close(t, o)
			if mode == "native" {
				o.WaczIndex = "warc"
			}
			e = o.Run(t.Context())
			count := captureCount(t, db)
			if mode == "broken" {
				if e == nil || count != 0 {
					t.Fatalf("broken index: count=%d error=%v", count, e)
				}
			} else if e != nil || count == 0 {
				t.Fatalf("count=%d error=%v", count, e)
			}
		})
	}
}

func TestWACZMemberPublicationAndEmptyReplacement(t *testing.T) {
	for _, mode := range []string{"auto", waczRawIndex} {
		t.Run(mode, func(t *testing.T) {
			fixture := fixtures(t)[0]
			name := filepath.Join(t.TempDir(), "members.wacz")
			db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: badgerFormat, Dir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer testcheck.Close(t, db)
			for _, both := range []bool{true, false} {
				const other = "second.warc"
				entries := map[string][]byte{
					waczArchiveDir + fixture.Name: fixture.Warc,
					waczArchiveDir + other:        nil,
					"indexes/index.cdxj":          []byte(fixture.Cdxj),
				}
				if both {
					entries[waczArchiveDir+other] = fixture.Warc
					lines := strings.TrimSpace(fixture.Cdxj) + "\n" + strings.ReplaceAll(fixture.Cdxj, fixture.Name, other)
					entries["indexes/index.cdxj"] = []byte(lines)
				}
				if err := os.WriteFile(name, packageBytes(t, entries), 0600); err != nil {
					t.Fatal(err)
				}
				opts := options(db, name)
				opts.WaczIndex = mode
				err := opts.Run(t.Context())
				if closeErr := opts.Close(); err != nil || closeErr != nil {
					t.Fatalf("index error=%v close error=%v", err, closeErr)
				}
				sources := map[string]int{}
				if err := db.Scan(t.Context(), "", "", func(c *cdx.Capture) error {
					sources[c.GetFilename()]++
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if sources[fixture.Name] == 0 || (sources[other] != 0) != both {
					t.Fatalf("both=%v, captures per member=%v", both, sources)
				}
			}
		})
	}
}

func TestWACZPreservesStoredPOSTKey(t *testing.T) {
	f := fixtures(t)[0]
	indexLine, _, _ := strings.Cut(f.Cdxj, "\n")
	_, fields, _ := strings.Cut(indexLine, " ")
	const key = "org,example)/form?__wb_method=post&field=value"
	wacz := filepath.Join(t.TempDir(), "post.wacz")
	if err := os.WriteFile(wacz, packageBytes(t, map[string][]byte{
		"archive/" + f.Name:  f.Warc,
		"indexes/index.cdxj": []byte(key + " " + fields + "\n"),
	}), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	if err := runPackage(t, db, wacz); err != nil {
		t.Fatal(err)
	}
	if err := db.Scan(t.Context(), "", "", func(c *cdx.Capture) error {
		if c.GetSsu() != key {
			t.Errorf("got key %q, want %q", c.GetSsu(), key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count := captureCount(t, db); count != 1 {
		t.Fatalf("got %d captures, want one", count)
	}
}

func TestContinuePastInvalidWACZ(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "a.wacz")
	if err := os.WriteFile(bad, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	f := fixtures(t)[0]
	if err := os.WriteFile(filepath.Join(dir, f.Name), f.Warc, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sink, err := cdx.NewOutput(&out, "-", "cdxj", "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, sink)
	o := options(sink, dir)
	defer testcheck.Close(t, o)
	o.ContinueOnError = true
	if err := o.Run(t.Context()); !errors.Is(err, zip.ErrFormat) {
		t.Fatalf("expected package error, got %v", err)
	}
	if !reflect.DeepEqual(entries(t, out.String()), entries(t, f.Cdxj)) {
		t.Fatalf("missing sibling captures: %s", out.String())
	}
}

func TestWACZInputHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell hooks")
	}
	dir := t.TempDir()
	name := filepath.Join(dir, "input.wacz")
	if err := os.WriteFile(name, leafPackage(t), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, exit string }{{"accept", "0"}, {"reject", "1"}, {"skip", "10"}, {"excluded", "1"}} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "closed")
			t.Setenv("HOOK_LOG", log)
			open := filepath.Join(dir, "open")
			closeHook := filepath.Join(dir, "close")
			if err := os.WriteFile(open, []byte("#!/bin/sh\nexit "+tc.exit+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(closeHook, []byte("#!/bin/sh\nprintf '%s' \"$WARC_FILE_NAME\" > \"$HOOK_LOG\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			sink, err := cdx.NewOutput(&out, "-", "cdxj", "", false)
			if err != nil {
				t.Fatal(err)
			}
			defer testcheck.Close(t, sink)
			o := options(sink, name)
			defer testcheck.Close(t, o)
			if tc.name == "excluded" {
				o.FileWalker.Suffixes = []string{".warc.gz"}
			}
			o.OpenInputFileHook, err = hooks.NewOpenInputFileHook("index", open)
			if err != nil {
				t.Fatal(err)
			}
			o.CloseInputFileHook, err = hooks.NewCloseInputFileHook("index", closeHook)
			if err != nil {
				t.Fatal(err)
			}
			err = o.Run(t.Context())
			if (err != nil) != (tc.name == "reject") || (out.Len() > 0) != (tc.name == "accept") {
				t.Fatalf("output %q, error %v", out.String(), err)
			}
			closed, err := os.ReadFile(log)
			if tc.name == "accept" {
				if err != nil || string(closed) != name {
					t.Fatalf("close hook: %q, %v", closed, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected close hook: %q, %v", closed, err)
			}
		})
	}
}

func TestWACZLongURLPolicies(t *testing.T) {
	fixture := fixtures(t)[0]
	dir := t.TempDir()
	name := filepath.Join(dir, "outer.wacz")
	var rows strings.Builder
	var keys []string
	for _, size := range []int{10, 59999, 60000, 60001, 100000, 100000} {
		key := strings.Repeat("x", size-1) + string(rune('a'+len(keys)))
		keys = append(keys, key)
		row := map[string]any{"url": "https://example.org/", "filename": fixture.Name, "offset": "0", "length": "1", "method": "POST", "requestBody": "x=" + strings.Repeat("a", 100000)}
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		rows.WriteString(key + " 20200101000000 " + string(b) + "\n")
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write([]byte(rows.String())); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	leaf := packageBytes(t, map[string][]byte{"archive/" + fixture.Name: fixture.Warc, "indexes/index.cdx.gz": compressed.Bytes()})
	if err := os.WriteFile(name, packageBytes(t, map[string][]byte{"leaf.wacz": leaf}), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sink, err := cdx.NewOutput(&out, "-", "cdxj", "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, sink)
	opts := options(sink, name)
	opts.LongURLKeys = "skip"
	if err := errors.Join(opts.Run(t.Context()), opts.Close()); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != len(keys) {
		t.Fatalf("exported %d keys", len(got))
	}
	sort.Strings(keys)
	for i, row := range got {
		if !strings.HasPrefix(row, keys[i]+" ") {
			t.Fatalf("changed key at row %d", i+1)
		}
	}
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Dir: t.TempDir(), Format: "badger"})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	for _, tc := range []struct {
		policy string
		want   int
	}{{"error", 0}, {"skip", 3}, {"preserve", 6}, {"skip", 3}} {
		o := options(db, name)
		o.LongURLKeys = tc.policy
		err := errors.Join(o.Run(t.Context()), o.Close())
		if tc.policy == "error" {
			if err == nil {
				t.Fatal("strict import succeeded")
			}
			for _, part := range []string{"outer.wacz", "leaf.wacz", "indexes/index.cdx.gz", "line 4", "60001"} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("missing %s in %v", part, err)
				}
			}
		} else if err != nil {
			t.Fatal(err)
		}
		if n := captureCount(t, db); n != tc.want {
			t.Fatalf("%s imported %d, want %d", tc.policy, n, tc.want)
		}
	}
}

func TestWACZDictionarySpillsInIndexTempDir(t *testing.T) {
	fixture := fixtures(t)[0]
	dir := t.TempDir()
	scratch := t.TempDir()
	var rows strings.Builder
	for i := range 90 {
		fmt.Fprintf(&rows, "%03d%s 20200101000000 {\"url\":\"https://example.org/\",\"filename\":%q,\"offset\":0,\"length\":1}\n", 89-i, strings.Repeat("x", 100000), fixture.Name)
	}
	name := filepath.Join(dir, "long.wacz")
	if err := os.WriteFile(name, packageBytes(t, map[string][]byte{"archive/" + fixture.Name: fixture.Warc, "indexes/index.cdxj": []byte(rows.String())}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(env, filepath.Join(dir, "missing"))
	}
	var out bytes.Buffer
	sink, err := cdx.NewOutput(&out, "-", "cdxj", scratch, false)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, sink)
	opts := options(sink, name)
	opts.tmpDir = scratch
	if err := errors.Join(opts.Run(t.Context()), opts.Close()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 90 || !sort.StringsAreSorted(lines) {
		t.Fatal("incomplete or unordered export")
	}
	if entries, err := os.ReadDir(scratch); err != nil || len(entries) != 0 {
		t.Fatalf("scratch directory after export: %v, %v", entries, err)
	}
}
