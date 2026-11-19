// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package filewalker

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	archivefs "github.com/nationallibraryofnorway/warchaeology/v5/internal/fs"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"github.com/spf13/afero"
)

type ownedFS struct {
	afero.Fs

	closed int
}

func (f *ownedFS) Close() error { f.closed++; return nil }
func TestOwnership(t *testing.T) {
	fs := &ownedFS{Fs: afero.NewMemMapFs()}
	w := New(WithFs(fs))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if fs.closed != 1 {
		t.Fatalf("close count = %d, want 1", fs.closed)
	}
	if err := w.Walk(context.Background(), "/", func(afero.Fs, string, error) error { return nil }); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("walk after close = %v", err)
	}
}
func zipData(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, err := z.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestNestedMountSurvivesTraversal(t *testing.T) {
	fs := afero.NewMemMapFs()
	if err := afero.WriteFile(fs, "outer.zip", zipData(t, "inner.zip", zipData(t, "record.warc", []byte("record"))), 0o644); err != nil {
		t.Fatal(err)
	}
	w := New(WithFs(fs), WithRecursive(true), WithSuffixes([]string{".warc"}))
	defer testcheck.Close(t, w)
	var consume func()
	if err := w.Walk(context.Background(), "outer.zip", func(mounted afero.Fs, name string, err error) error {
		if err != nil {
			return err
		}
		consume = func() {
			data, err := afero.ReadFile(mounted, name)
			if err != nil || string(data) != "record" {
				t.Errorf("delayed read = %q, %v", data, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if consume == nil {
		t.Fatal("no record found")
	}
	consume()
}
func TestMountErrorsReachCallback(t *testing.T) {
	fs := afero.NewMemMapFs()
	if err := afero.WriteFile(fs, "bad.zip", []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := New(WithFs(fs), WithSuffixes([]string{".warc"}))
	defer testcheck.Close(t, w)
	sentinel := errors.New("callback error")
	called := false
	err := w.Walk(context.Background(), "bad.zip", func(_ afero.Fs, _ string, err error) error {
		called = true
		if err == nil {
			t.Error("mount error swallowed")
		}
		return sentinel
	})
	if !called || !errors.Is(err, sentinel) {
		t.Fatalf("called=%v err=%v", called, err)
	}
}

var _ io.Closer = (*FileWalker)(nil)

func TestExplicitArchiveMember(t *testing.T) {
	fs := afero.NewMemMapFs()
	if err := afero.WriteFile(fs, "/sample.wacz", zipData(t, "archive/record.warc", []byte("record")), 0600); err != nil {
		t.Fatal(err)
	}
	mounted, err := archivefs.ResolveFilesystem(fs, "/sample.wacz")
	if err != nil {
		t.Fatal(err)
	}
	walker := New(WithFs(mounted), WithSuffixes([]string{".warc"}))
	defer testcheck.Close(t, walker)
	count := 0
	err = walker.Walk(t.Context(), "/archive/record.warc", func(_ afero.Fs, path string, err error) error {
		if err != nil {
			return err
		}
		if path != "/archive/record.warc" {
			t.Fatalf("unexpected member: %s", path)
		}
		count++
		return nil
	})
	if err != nil || count != 1 {
		t.Fatalf("count=%d error=%v", count, err)
	}
}
