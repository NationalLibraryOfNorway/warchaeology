// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || freebsd || linux || openbsd || solaris || windows

package util

import (
	"path/filepath"
	"testing"
)

func TestDiskFree(t *testing.T) {
	dir := t.TempDir()
	free, err := DiskFree(dir)
	if err != nil {
		t.Fatalf("DiskFree(%q): %v", dir, err)
	}
	if free < 0 {
		t.Errorf("DiskFree(%q) = %d, want nonnegative bytes", dir, free)
	}

	missing := filepath.Join(dir, "missing")
	if _, err := DiskFree(missing); err == nil {
		t.Errorf("DiskFree(%q) succeeded for a missing directory", missing)
	}
}
