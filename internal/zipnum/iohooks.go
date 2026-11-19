// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"bufio"
	"io"
	"os"
)

// These narrow boundaries let tests observe part reads and inject durability failures.
var (
	openPartFile = os.Open
	flushBuffer  = func(w *bufio.Writer) error { return w.Flush() }
	syncFile     = func(f *os.File) error { return f.Sync() }
	closeIO      = func(c io.Closer) error { return c.Close() }
)
