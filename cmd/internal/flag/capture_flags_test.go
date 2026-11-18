// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package flag

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultCaptureDirectory(t *testing.T) {
	home := t.TempDir()
	data := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("LOCALAPPDATA", data)

	wantBase := data
	if runtime.GOOS == "darwin" {
		wantBase = filepath.Join(home, "Library", "Application Support")
	}
	check := func(base string) {
		t.Helper()
		got, err := defaultCaptureDirectory()
		want := filepath.Join(base, "warchaeology", "warcdb")
		if err != nil || got != want {
			t.Fatalf("defaultCaptureDirectory() = %q, %v; want %q", got, err, want)
		}
	}
	check(wantBase)

	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("LOCALAPPDATA", "")
	switch runtime.GOOS {
	case "windows":
		wantBase = filepath.Join(home, "AppData", "Local")
	case "darwin":
		wantBase = filepath.Join(home, "Library", "Application Support")
	default:
		wantBase = filepath.Join(home, ".local", "share")
	}
	check(wantBase)

	// Relative XDG paths must not make the database depend on the working directory.
	t.Setenv("XDG_DATA_HOME", "relative-data")
	check(wantBase)
}
