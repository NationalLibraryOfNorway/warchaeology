package index

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
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
	entry.Write(fixture.Warc)
	// Use a native export so offsets and filenames match this exact member.
	line := fixture.Cdxj
	entry, e = z.Create("indexes/index.cdxj")
	if e != nil {
		t.Fatal(e)
	}
	entry.Write([]byte(line))
	z.Close()
	f.Close()
	db, e := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	o := options(db, name)
	defer o.Close()
	if e = o.Run(context.Background()); e != nil {
		t.Fatal(e)
	}
	n := 0
	e = db.Scan(t.Context(), "", "", func(c *cdx.Capture) error {
		n++
		var src map[string]any
		if e := json.Unmarshal([]byte(c.Source), &src); e != nil {
			return e
		}
		rec, e := (loader.FileStorageLoader{}).RecordByStorageRef(t.Context(), c.Ref)
		if e != nil {
			return e
		}
		return rec.Close()
	})
	if e != nil || n == 0 {
		t.Fatalf("captures=%d error=%v", n, e)
	}
	o2 := options(db, name)
	defer o2.Close()
	if e = o2.Run(t.Context()); e != nil {
		t.Fatal(e)
	}
	count := 0
	db.Scan(t.Context(), "", "", func(*cdx.Capture) error { count++; return nil })
	if count != n {
		t.Fatalf("repeat changed count: %d != %d", count, n)
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
					g.Write(content)
					g.Close()
					content = b.Bytes()
				}
				w, e = z.Create(n)
				if e != nil {
					t.Fatal(e)
				}
				w.Write(content)
			}
			if e = z.Close(); e != nil {
				t.Fatal(e)
			}
			f.Close()
			db, e := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: t.TempDir()})
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			o := options(db, name)
			defer o.Close()
			if mode == "native" {
				o.WaczIndex = "warc"
			}
			e = o.Run(t.Context())
			count := 0
			db.Scan(t.Context(), "", "", func(*cdx.Capture) error { count++; return nil })
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
	defer sink.Close()
	o := options(sink, dir)
	defer o.Close()
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
			defer sink.Close()
			o := options(sink, name)
			defer o.Close()
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
