// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package diskspace

import "golang.org/x/sys/unix"

func Free(path string) (int64, error) {
	var stat unix.Statvfs_t
	if err := unix.Statvfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bfree * stat.Frsize), nil
}
