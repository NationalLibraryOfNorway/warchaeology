// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"bytes"
)

var ipKey = []byte("ip")

// getPartitionKey creates the partitioning key from a CDXJ line.
//
// Lines with the same key are grouped together in consecutive partition files.
func getPartitionKey(line []byte) []byte {
	if len(line) == 0 {
		return nil
	}
	b := line[0]
	// If the first byte is a digit, a colon, or an opening square bracket,
	// we assume it's an IP address (IPv4 or IPv6).
	if (b >= '0' && b <= '9') || b == ':' || b == '[' {
		return ipKey
	}
	// Find the first space to separate the SURT from the rest of the line
	before, _, ok := bytes.Cut(line, []byte{' '})
	if !ok {
		// invalid cdxj line
		return nil
	}
	rparen := bytes.IndexByte(before, ')')
	if rparen == -1 {
		// Scheme-style keys group by scheme (dns:, warcinfo:, mailto:, etc.).
		colon := bytes.IndexByte(before, ':')
		if colon > -1 {
			return line[:colon]
		}

		// invalid cdxj line
		return nil
	}

	// SURT keys group by top-level domain.

	// Locate the first comma (partition on TLD)
	comma := bytes.IndexByte(line[:rparen], ',')
	if comma == -1 {
		// If there is no comma, return the thing up to the closing parenthesis
		return line[:rparen]
	}
	return line[:comma]
}

// lineFilter retains records with a SURT or scheme-style URL key.
// It is a structural filter, not a full CDXJ or JSON validator.
func lineFilter(line []byte) bool {
	// Invalid line
	if len(line) < 4 {
		return false
	}

	// Must have a space separating urlkey and json-ish payload.
	space := bytes.IndexByte(line, ' ')
	if space <= 0 {
		return false
	}
	first := line[:space]

	// The URL key must look like either:
	//    - SURT-host form containing ')', or
	//    - scheme-style key containing ':' (dns:, warcinfo:, filedesc:, mailto:, etc.)
	if !bytes.ContainsAny(first, "):") {
		return false
	}

	return true
}
