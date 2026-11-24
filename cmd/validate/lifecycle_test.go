// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"errors"
	"os"
	"testing"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/spf13/afero"
)

type closingFS struct {
	afero.Fs

	closed bool
}

func (fs *closingFS) Close() error { fs.closed = true; return nil }
func TestRunReturnsMountFailureAndLeavesCleanupToCaller(t *testing.T) {
	fs := &closingFS{Fs: afero.NewMemMapFs()}
	if err := afero.WriteFile(fs, "bad.zip", []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := &ValidateOptions{FileWalker: filewalker.New(filewalker.WithFs(fs)), paths: []string{"bad.zip"}}
	t.Cleanup(func() {
		if err := o.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := o.Run(); err == nil {
		t.Fatal("mount failure not returned")
	}
	if fs.closed {
		t.Fatal("Run closed the caller-owned input filesystem")
	}
}
func TestRunReturnsTraversalFailure(t *testing.T) {
	o := &ValidateOptions{FileWalker: filewalker.New(filewalker.WithFs(afero.NewMemMapFs())), paths: []string{"missing.warc"}}
	t.Cleanup(func() {
		if err := o.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := o.Run(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Run = %v", err)
	}
}
