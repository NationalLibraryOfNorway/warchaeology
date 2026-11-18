// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package publication

import "golang.org/x/sys/windows"

// Windows persists the directory change through MoveFileEx's write-through flag.
func SyncDir(string) error { return nil }

func Rename(src, dst string, replace bool) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if replace {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	return windows.MoveFileEx(from, to, flags)
}
