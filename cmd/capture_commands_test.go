// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipnum"
	"github.com/spf13/viper"
)

func TestZipnumSubcommands(t *testing.T) {
	t.Cleanup(viper.Reset)
	dir := t.TempDir()
	input := filepath.Join(dir, "part,2.cdxj")
	if err := os.WriteFile(input, []byte("com,example)/ 20200101000000 {\"url\":\"https://example.com/\",\"filename\":\"a.warc.gz\",\"offset\":\"0\",\"length\":\"100\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing*.cdxj")
	unpublished := filepath.Join(dir, "unpublished")
	if err := executeCaptureCommand(t, "zipnum", "build", input, missing, "--output-dir", unpublished); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("unmatched glob: %v", err)
	}
	if _, err := os.Stat(unpublished); !os.IsNotExist(err) {
		t.Fatalf("partial cluster: %v", err)
	}
	first, added := filepath.Join(dir, "first"), filepath.Join(dir, "added")
	merged, repartitioned := filepath.Join(dir, "merged"), filepath.Join(dir, "repartitioned")
	for _, tt := range []struct {
		args   []string
		output string
		count  int
	}{
		{[]string{"build", input, "--lines", "1", "--blocks", "1"}, first, 1},
		{[]string{"add", first, input}, added, 2},
		{[]string{"merge", first, added}, merged, 3},
		{[]string{"repartition", merged, "--lines", "2", "--blocks", "2"}, repartitioned, 3},
	} {
		viper.Reset()
		command := NewWarcCommand()
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		args := append([]string{"zipnum"}, tt.args...)
		command.SetArgs(append(args, "--output-dir", tt.output))
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		reader, err := zipnum.Open(tt.output)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		err = reader.Scan(context.Background(), "", "", func(*cdx.Capture) error { count++; return nil })
		closeErr := reader.Close()
		if err != nil || closeErr != nil || count != tt.count {
			t.Fatalf("%s: captures=%d want=%d, scan=%v close=%v", tt.args[0], count, tt.count, err, closeErr)
		}
	}
}

func TestCaptureCommandValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"readonly inputs", []string{"serve", "--read-only", "capture.warc"}, nil, "rejects ingestion"},
		{"zipnum inputs", []string{"serve", "--index-format", "zipnum", "capture.warc"}, nil, "does not accept ingestion"},
		{"zipnum database flag", []string{"serve", "--index-format", "zipnum", "--badger-dir", "db"}, nil, "badger-dir is not valid"},
		{"zipnum database environment", []string{"serve", "--index-format", "zipnum"}, map[string]string{"WARC_BADGER_DIR": "db"}, "badger-dir is not valid"},
		{"zipnum layout environment", []string{"zipnum", "build", "missing.cdxj"}, map[string]string{"WARC_LINES": "0"}, "line and block counts must be positive"},
		{"zipnum merge inputs", []string{"zipnum", "merge", "missing"}, nil, "accepts 2 arg(s)"},
		{"zipnum add inputs", []string{"zipnum", "add", "missing"}, nil, "accepts 2 arg(s)"},
		{"zipnum repartition inputs", []string{"zipnum", "repartition", "one", "two"}, nil, "accepts 1 arg(s)"},
		{"invalid prefix", []string{"serve", "--path-prefix", "invalid"}, nil, "path-prefix must start"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			command := NewWarcCommand()
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			command.SetArgs(tt.args)
			if err := command.Execute(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func executeCaptureCommand(t *testing.T, args ...string) error {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	command := NewWarcCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs(args)
	return command.Execute()
}

func TestRedupeSymlinkContainment(t *testing.T) {
	source := t.TempDir()
	inside := filepath.Join(source, "published")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(source, alias); err != nil {
		t.Skip(err)
	}
	for _, pair := range [][2]string{{source, filepath.Join(alias, "published")}, {alias, inside}} {
		if err := executeCaptureCommand(t, "redupe", "-r", "--output-dir", pair[1], pair[0]); err == nil || !strings.Contains(err.Error(), "outside source") {
			t.Fatalf("overlapping paths: %v", err)
		}
	}
	entries, err := os.ReadDir(inside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("published entries %v, %v", entries, err)
	}
}

func TestFulltextRejectsBeforeIngestion(t *testing.T) {
	opts := cdxstore.Options{Dir: t.TempDir(), Format: "badger", IndexOnly: true}
	db, err := cdxstore.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	const row = `org,example)/ 20200101000000 {"url":"https://example.org/","filename":"old.warc","offset":"0","length":"1"}`
	if _, err = db.Upload(t.Context(), strings.NewReader(row), cdxstore.UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	var before, after bytes.Buffer
	if err = db.Export(t.Context(), &before); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = executeCaptureCommand(t, "index", "--fulltext", "--index-format=badger", "--badger-dir", opts.Dir, "../testdata/warc/convert.warc"); err == nil {
		t.Fatal("accepted index-only database")
	}
	db, err = cdxstore.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err = db.Export(t.Context(), &after); err != nil || after.String() != before.String() {
		t.Fatal(err, after.String())
	}
	stats, err := db.Stats(t.Context())
	if err != nil || stats["captures"] != int64(1) {
		t.Fatal(err, stats)
	}
}
