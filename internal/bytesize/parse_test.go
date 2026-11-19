// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package bytesize

import "testing"

func TestParse(t *testing.T) {
	for input, want := range map[string]int64{
		"123": 123, "25B": 25, "2kb": 2 << 10, " 12 mb ": 12 << 20,
		"1GB": 1 << 30, "1TB": 1 << 40, "1PB": 1 << 50,
		"": 0, "-1MB": 0, "invalid": 0, "2KiB": 0, "8192PB": 0,
	} {
		if got := Parse(input); got != want {
			t.Errorf("Parse(%q) = %d, want %d", input, got, want)
		}
	}
}
