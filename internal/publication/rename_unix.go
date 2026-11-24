//go:build !windows

// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package publication

import (
	"errors"
	"os"
)

func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func Rename(src, dst string, replace bool) error {
	if !replace {
		if _, err := os.Lstat(dst); err == nil {
			return &os.PathError{Op: "rename", Path: dst, Err: os.ErrExist}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Rename(src, dst)
}
