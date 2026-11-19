// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package keyvalue

import "testing"

func TestCanonicalKey(t *testing.T) {
	key := CdxKey("com,example)/path 20200102030405 source 00000000000000000042 generation")
	if key.Domain() != "com,example" || key.Path() != "/path" || key.Time().Year() != 2020 {
		t.Fatalf("invalid key interpretation: %s", key)
	}
	if deSurtDomain(key.Domain()) != "example.com" {
		t.Fatal("domain")
	}
}
