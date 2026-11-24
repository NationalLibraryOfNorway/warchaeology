// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package diskspace

import "golang.org/x/sys/unix"

func Free(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return max(stat.F_bavail, 0) * int64(stat.F_bsize), nil
}
