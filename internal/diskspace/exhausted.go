// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package diskspace

import (
	"errors"
	"syscall"
)

// Exhausted reports whether storage space or disk quota has run out.
func Exhausted(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
