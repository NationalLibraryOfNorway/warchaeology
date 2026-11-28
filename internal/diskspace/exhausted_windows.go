// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package diskspace

import (
	"errors"
	"golang.org/x/sys/windows"
	"syscall"
)

// Exhausted reports whether storage space or disk quota has run out.
func Exhausted(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) || errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) || errors.Is(err, windows.ERROR_DISK_QUOTA_EXCEEDED)
}
