// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package timestamp

import "time"

const layout14 = "20060102150405"

// Format14 converts a time.Time to a 14 digit string according to the ARC file format.
func Format14(t time.Time) string {
	return t.UTC().Format(layout14)
}

// FormatNano formats a UTC ordering key with nine fractional digits.
func FormatNano(t time.Time) string {
	return t.UTC().Format(layout14 + ".000000000")
}

// FormatRFC3339 formats an instant in UTC with up to nanosecond precision.
func FormatRFC3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
