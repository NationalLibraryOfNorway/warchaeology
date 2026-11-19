// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import "testing"

func FuzzCDXJHelpers(f *testing.F) {
	for _, seed := range []string{
		"", "com,example)/ 20200101000000 {}\n", "::1)/ 1 {}\n", "dns:example 1 {}\n", "malformed",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		_ = lineFilter(line)
		_ = getPartitionKey(line)
		_, _ = cdxIndexKey(line)
	})
}

func FuzzIndexParser(f *testing.F) {
	for _, seed := range []string{
		"key\tpart-00000.gz\t0\t1\t1\n",
		"key\tpart-01890f12-3456-7000-8000-000000000001.gz\t0\t42\t1\n",
		"../../escape\tbad\t-1\t0\n",
		"",
	} {
		f.Add([]byte(seed), true)
	}
	f.Fuzz(func(t *testing.T, line []byte, cluster bool) {
		_, _, _ = parseIndexFields(line, cluster)
	})
}
