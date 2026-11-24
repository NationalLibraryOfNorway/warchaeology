// Copyright 2020 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package timestamp

import (
	"fmt"
	"time"
)

// Parse14 parses a complete ARC or CDX timestamp in UTC.
func Parse14(s string) (time.Time, error) {
	if len(s) != len(layout14) {
		return time.Time{}, fmt.Errorf("invalid archive timestamp: %s", s)
	}
	return ParseKey(s)
}

// ParseKey reads the whole or fractional seconds in a native capture key.
func ParseKey(s string) (time.Time, error) {
	return time.Parse(layout14, s)
}

// ParsePartial accepts a CDX query timestamp from year through second precision.
func ParsePartial(s string) (time.Time, error) {
	n := len(s)
	if n < 4 || n > len(layout14) || n%2 != 0 {
		return time.Time{}, fmt.Errorf("invalid CDX timestamp: %s", s)
	}
	return time.Parse(layout14[:n], s)
}

// Distance returns the absolute difference across the full CDX date range.
func Distance(a, b time.Time) (seconds int64, nanos int) {
	if a.Before(b) {
		a, b = b, a
	}
	seconds, nanos = a.Unix()-b.Unix(), a.Nanosecond()-b.Nanosecond()
	if nanos < 0 {
		seconds--
		nanos += 1_000_000_000
	}
	return
}
