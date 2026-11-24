// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || freebsd || linux || openbsd || solaris || windows

package diskspace

import (
	"path/filepath"
	"testing"
)

func TestFree(t *testing.T) {
	dir := t.TempDir()
	free, err := Free(dir)
	if err != nil {
		t.Fatalf("Free(%q): %v", dir, err)
	}
	if free < 0 {
		t.Errorf("Free(%q) = %d, want nonnegative bytes", dir, free)
	}

	missing := filepath.Join(dir, "missing")
	if _, err := Free(missing); err == nil {
		t.Errorf("Free(%q) succeeded for a missing directory", missing)
	}
}
