// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package widget

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestContentPreservesPartialRead(t *testing.T) {
	failure := errors.New("broken block")
	for _, visible := range []bool{false, true} {
		// Expanded newlines exceed io.Copy's buffer and leave transformed bytes
		// pending when the source returns its final data and error together.
		block := strings.Repeat("\n", 16384) + "last bytes"
		src := iotest.DataErrReader(io.MultiReader(strings.NewReader(block), iotest.ErrReader(failure)))
		var dst strings.Builder
		err := writeContent(&dst, strings.NewReader("header\r\n"), src, visible)
		if !errors.Is(err, failure) || !strings.HasSuffix(dst.String(), "last bytes") {
			t.Fatalf("visible=%t: %v, %d bytes", visible, err, dst.Len())
		}
	}
}
