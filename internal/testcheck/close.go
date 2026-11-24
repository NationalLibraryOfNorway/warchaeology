// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

// Package testcheck provides resource checks shared by tests and benchmarks.
package testcheck

import (
	"io"
	"testing"
)

// Close reports a cleanup failure while allowing other checks to run.
func Close(t testing.TB, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Errorf("close %T: %v", c, err)
	}
}
