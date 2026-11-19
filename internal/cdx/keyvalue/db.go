// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package keyvalue

import (
	"strings"
	"time"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
)

// CdxKey is a canonical SURT, timestamp and source-position identity.
type CdxKey []byte

func (k CdxKey) String() string { return string(k) }

func (k CdxKey) urlkey() string { s, _, _ := strings.Cut(string(k), " "); return s }

func (k CdxKey) Domain() string { host, _, _ := strings.Cut(k.urlkey(), ")"); return host }

func (k CdxKey) Path() string { _, path, _ := strings.Cut(k.urlkey(), ")"); return path }

func (k CdxKey) Time() time.Time {
	fields := strings.Fields(string(k))
	if len(fields) < 2 {
		return time.Time{}
	}
	t, _ := timestamp.ParseKey(fields[1])
	return t
}

func (k CdxKey) Unix() int64 { return k.Time().Unix() }

func deSurtDomain(domain string) string {
	parts := strings.Split(strings.Trim(domain, ","), ",")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ".")
}
