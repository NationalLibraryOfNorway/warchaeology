// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warcwriterconfig

import (
	"strings"
	"testing"
	"time"
)

func Test_parseSubdirPattern(t *testing.T) {
	tests := []struct {
		name       string
		dirPattern string
		recordDate string
		want       string
	}{
		{"no template", "", "2006-11-17T12:02:28Z", ""},
		{"4 digit year", "{YYYY}", "2006-11-17T12:02:28Z", "2006"},
		{"2 digit year", "{YY}", "2006-11-17T12:02:28Z", "06"},
		{"month", "{MM}", "2006-11-17T12:02:28Z", "11"},
		{"day", "{DD}", "2006-11-17T12:02:28Z", "17"},
		{"year month day", "{YYYY}/{MM}/{DD}", "2006-11-17T12:02:28Z", "2006/11/17"},
		{"extra path", "01/{YYYY}/{MM}/{DD}", "2006-11-17T12:02:28Z", "01/2006/11/17"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recordDate, err := time.Parse(time.RFC3339, tt.recordDate)
			if err != nil {
				t.Fatalf("failed to parse record date: %v", err)
			}
			got := parseSubdirPattern(tt.dirPattern, recordDate)
			if got != tt.want {
				t.Errorf("parseSubdirPattern() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultNamerKeepsOutputConfiguration(t *testing.T) {
	first := NewDefaultNamer("first-", t.TempDir())
	dir := t.TempDir()
	second := NewDefaultNamer("second-", dir)
	firstDir, firstName := first.NewWarcfileName()
	secondDir, secondName := second.NewWarcfileName()
	if firstDir == secondDir || secondDir != dir || !strings.HasPrefix(firstName, "first-") || !strings.HasPrefix(secondName, "second-") {
		t.Fatalf("names = %q/%q and %q/%q", firstDir, firstName, secondDir, secondName)
	}
}
