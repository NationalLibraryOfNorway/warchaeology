// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseStorageRef parses a storageRef (eg. warcfile:filename#offset) into parts.
func ParseStorageRef(storageRef string) (string, int64, error) {
	n := strings.IndexRune(storageRef, ':')
	if n == -1 {
		return "", 0, fmt.Errorf("invalid storage ref, missing scheme delimiter ':': '%s'", storageRef)
	}
	// scheme := ref[:n]
	// if scheme != "warcfile" {
	// 	err = fmt.Errorf("invalid storage ref '%s', scheme must be \"warcfile\", was: %s", ref, scheme)
	// 	return
	// }
	ref := storageRef[n+1:]
	n = strings.LastIndexByte(ref, '#')
	if n == -1 {
		return "", 0, fmt.Errorf("invalid storage ref, missing offset delimiter '#': '%s'", storageRef)
	}
	filename := ref[:n]
	offset, err := strconv.ParseInt(ref[n+1:], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("invalid storage ref, failed to parse offset: '%s': %w", storageRef, err)
	}
	return filename, offset, nil
}
