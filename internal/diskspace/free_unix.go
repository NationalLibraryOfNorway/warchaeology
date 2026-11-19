// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || freebsd || linux

package diskspace

import "golang.org/x/sys/unix"

func Free(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(max(stat.Bavail, 0)) * int64(stat.Bsize), nil //nolint:unconvert // Statfs_t field types vary by platform.
}
