// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warcwriterconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSyncFailureRollsBackPublication(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "staged")
	dest := filepath.Join(dir, "out.warc")
	if err := os.WriteFile(stage, []byte("output"), 0600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("directory sync failed")
	err := publishStaged(stage, dest, root, func(path string) error {
		if path == root {
			return failure
		}
		return nil
	}, func() error { t.Fatal("output hook ran before durable publication"); return nil })
	if !errors.Is(err, failure) {
		t.Fatalf("publication returned %v", err)
	}
	if _, err = os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination after failed sync: %v", err)
	}
}
