// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warc

import (
	"fmt"

	"github.com/nationallibraryofnorway/gowarc/v4"
)

func ErrorFrom(record gowarc.Record, err error) RecordError {
	return RecordError{offset: record.Offset, Err: err}
}

type RecordError struct {
	offset int64
	Err    error
}

func (e RecordError) Unwrap() error {
	return e.Err
}

func (e RecordError) Error() string {
	return fmt.Sprintf("offset %d: %v", e.offset, e.Err)
}

func (e RecordError) Offset() int64 {
	return e.offset
}
