// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warcwriterconfig

import (
	"errors"
	"os"
	"path/filepath"
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

func TestReserveIdentityOutput(t *testing.T) {
	dir := t.TempDir()
	cfg, err := New("dedup", WithOutDir(dir), WithOneToOneWriter(true), WithFilePrefix("pre-"), WithSubDirPattern("{YYYY}"))
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	release, err := cfg.ReserveOutput("a/item.warc.gz", date)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ReserveOutput("b/item.warc", date); !errors.Is(err, os.ErrExist) {
		t.Fatalf("duplicate reservation %v", err)
	}
	other, err := cfg.ReserveOutput("b/item.warc", date.AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := other(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2022", "pre-item.warc.gz")
	if err := os.WriteFile(path, []byte("published"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ReserveOutput("b/item.warc", date); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing output %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "published" {
		t.Fatalf("published output %q %v", b, err)
	}
}
