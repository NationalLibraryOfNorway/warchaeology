// Copyright 2023 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package bytesize

import (
	"strings"
	"unicode"

	"github.com/spf13/cast"
)

// Parse converts size strings such as "1GB" and "12 mb" to a byte count.
func Parse(sizeStr string) int64 {
	sizeStr = strings.TrimSpace(sizeStr)
	lastChar := len(sizeStr) - 1
	multiplier := int64(1)

	if lastChar > 0 {
		if sizeStr[lastChar] == 'b' || sizeStr[lastChar] == 'B' {
			if lastChar > 1 {
				switch unicode.ToLower(rune(sizeStr[lastChar-1])) {
				case 'k':
					multiplier = 1 << 10
					sizeStr = strings.TrimSpace(sizeStr[:lastChar-1])
				case 'm':
					multiplier = 1 << 20
					sizeStr = strings.TrimSpace(sizeStr[:lastChar-1])
				case 'g':
					multiplier = 1 << 30
					sizeStr = strings.TrimSpace(sizeStr[:lastChar-1])
				case 't':
					multiplier = 1 << 40
					sizeStr = strings.TrimSpace(sizeStr[:lastChar-1])
				case 'p':
					multiplier = 1 << 50
					sizeStr = strings.TrimSpace(sizeStr[:lastChar-1])
				default:
					multiplier = 1
					sizeStr = strings.TrimSpace(sizeStr[:lastChar])
				}
			}
		}
	}

	size := max(cast.ToInt64(sizeStr), 0)

	return safeMul(size, multiplier)
}

func safeMul(a, b int64) int64 {
	c := a * b
	if a > 1 && b > 1 && c/b != a {
		return 0
	}
	return c
}
