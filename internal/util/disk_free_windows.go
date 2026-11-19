// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package util

import "golang.org/x/sys/windows"

func DiskFree(path string) (int64, error) {
	dir, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	// Preserve the volume-wide free space measurement when user quotas apply.
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(dir, nil, nil, &free); err != nil {
		return 0, err
	}
	return int64(free), nil
}
