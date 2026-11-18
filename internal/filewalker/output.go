// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package filewalker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/afero"
)

// CheckOutputPaths protects local inputs before callers create or replace output.
// Mounted members share their outer container's identity on the host filesystem.
func (fw *FileWalker) CheckOutputPaths(ctx context.Context, inputs []string, outputs ...string) error {
	targets := make(map[string]os.FileInfo, len(outputs))
	for _, output := range outputs {
		info, err := os.Stat(output)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		targets[output] = info
	}
	if len(targets) == 0 {
		return nil
	}
	check := func(input string) error {
		info, err := os.Stat(input)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for output, target := range targets {
			if os.SameFile(info, target) {
				return fmt.Errorf("output %q aliases archive input %q", output, input)
			}
		}
		return nil
	}
	if fw.SourceRoot != "" {
		if root := localRootPath(fw.SourceRoot); root != "" {
			return check(root)
		}
		return nil
	}
	if fw.Fs.Name() != "OsFs" {
		return nil
	}
	probe := New(WithFs(afero.NewOsFs()), WithRecursive(fw.Recursive), WithFollowSymlinks(fw.FollowSymlinks))
	probe.ArchiveHandler = func(_ context.Context, _ afero.Fs, name string) (bool, error) {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".wacz", ".zip", ".tar", ".tgz":
			return true, check(name)
		}
		if fw.HasSuffix(name) || strings.HasSuffix(name, ".tar.gz") {
			return true, check(name)
		}
		return true, nil
	}
	for _, input := range inputs {
		if err := check(input); err != nil {
			return err
		}
		if err := probe.Walk(ctx, input, func(_ afero.Fs, _ string, err error) error {
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}
