// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !freebsd && !linux && !openbsd && !solaris && !windows

package diskspace

import "errors"

func Free(string) (int64, error) {
	return 0, errors.ErrUnsupported
}
