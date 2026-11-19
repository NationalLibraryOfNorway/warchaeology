// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
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
		{"zipnum database flag", []string{"serve", "--index-format", "zipnum", "--index-dir", "db"}, nil, "index-dir is not valid"},
		{"zipnum database environment", []string{"serve", "--index-format", "zipnum"}, map[string]string{"WARC_INDEX_DIR": "db"}, "index-dir is not valid"},
		{"badger export filename", []string{"index", "--index-format", "badger", "--index-file", "-"}, nil, "index-file requires"},
		{"file export database directory", []string{"index", "capture.warc"}, map[string]string{"WARC_INDEX_DIR": "db"}, "index-dir requires"},
		{"serve export filename", []string{"serve", "--index-file", "captures.cdxj"}, nil, "index-file is not valid"},
		{"old export flag", []string{"index", "--index-out", "captures.cdxj"}, nil, "unknown flag: --index-out"},
		{"old database flag", []string{"index", "--badger-dir", "db"}, nil, "unknown flag: --badger-dir"},
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

func TestArchiveOutputsRejectInputAliases(t *testing.T) {
	warc, err := os.ReadFile("../testdata/warc/single-record.warc")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index direct", "index directory", "index source list", "redupe archive", "redupe findings", "redupe explicit mount"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.warc")
			if name == "redupe findings" {
				source = filepath.Join(dir, "report.json.findings.jsonl")
			}
			input := warc
			if name == "redupe archive" || name == "redupe explicit mount" {
				source = filepath.Join(dir, "bundle.tar")
				var data bytes.Buffer
				w := tar.NewWriter(&data)
				if err := w.WriteHeader(&tar.Header{Name: "source.warc", Size: int64(len(warc)), Mode: 0600}); err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write(warc); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				input = data.Bytes()
			}
			if err := os.WriteFile(source, input, 0600); err != nil {
				t.Fatal(err)
			}
			var args []string
			switch name {
			case "index direct":
				args = []string{"index", "--index-file", source, source}
			case "index directory":
				alias := filepath.Join(dir, "index.cdxj")
				if err := os.Link(source, alias); err != nil {
					t.Fatal(err)
				}
				args = []string{"index", "--index-file", alias, dir}
			case "index source list":
				alias := filepath.Join(dir, "index.cdxj")
				if err := os.Symlink(source, alias); err != nil {
					t.Skipf("creating a symlink: %v", err)
				}
				list := filepath.Join(dir, "sources.txt")
				if err := os.WriteFile(list, []byte(source+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				args = []string{"index", "--index-file", alias, "--source-file-list", list}
			case "redupe archive":
				args = []string{"redupe", "--audit", "--report", source, source}
			case "redupe findings":
				args = []string{"redupe", "--audit", "--report", filepath.Join(dir, "report.json"), source}
			case "redupe explicit mount":
				args = []string{"redupe", "--audit", "--report", source, "--input-file", "tar://" + source, "/"}
			}
			if err := executeCaptureCommand(t, args...); err == nil || !strings.Contains(err.Error(), "aliases archive input") {
				t.Fatalf("expected archive collision, got %v", err)
			}
			data, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(data, input) {
				t.Fatalf("source changed: %v", err)
			}
		})
	}
}

func TestDedupPublicationFailureLeavesIndexesUncommitted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture checks Unix rename errors")
	}
	const source = "../testdata/warc/single-record.warc"
	for _, mode := range []string{"--one-to-one", "--name-generator=identity"} {
		t.Run(mode, func(t *testing.T) {
			out, work := t.TempDir(), t.TempDir()
			if err := os.Mkdir(filepath.Join(out, "single-record.warc"), 0750); err != nil {
				t.Fatal(err)
			}
			args := []string{"dedup", "--output-dir", out, "--index-dir", work, "--keep-index", "--compress=false",
				"--min-disk-free=0", "--min-index-disk-free=0", "--concurrent-writers=1", mode, source}
			if err := executeCaptureCommand(t, args...); err == nil || !strings.Contains(err.Error(), "rename") {
				t.Fatalf("expected a publication error, got %v", err)
			}
			donor, err := index.NewDigestIndex(work, true, false)
			if donor != nil {
				t.Cleanup(func() { _ = donor.Close() })
			}
			if err == nil || !strings.Contains(err.Error(), "unfinished dedup output") {
				t.Fatalf("expected the donor index to retain its unfinished marker, got %v", err)
			}
			files, err := index.NewFileIndex(work, true, false)
			if err != nil {
				t.Fatal(err)
			}
			defer files.Close()
			for _, key := range []string{source, filepath.Clean(source)} {
				if cached, err := files.GetFileStats(key); err != nil || cached != nil {
					t.Fatalf("file cached after failed publication: %v, %v", cached, err)
				}
			}
		})
	}
}

func TestIndexDestinationConfiguration(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "warc.yaml")
	db := filepath.Join(root, "db")
	if err := os.WriteFile(config, []byte("index-format: badger\nindex-dir: "+filepath.ToSlash(db)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := executeCaptureCommand(t, "--config", config, "index", "../testdata/warc/single-record.warc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(db, "MANIFEST")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "captures.cdxj")
	t.Setenv("WARC_INDEX_FILE", file)
	if err := executeCaptureCommand(t, "index", "../testdata/warc/single-record.warc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("export file was not created: %v", err)
	}
}

func TestWACZRevalidatesAfterParserConfigChange(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "collection.wacz")
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	member, err := z.Create("archive/data.warc")
	if err != nil {
		t.Fatal(err)
	}
	const record = "WARC/1.1\r\nWARC-Type: resource\r\nWARC-Date: 2026-11-17T00:00:00Z\r\nWARC-Record-ID: <urn:example:original>\r\nWARC-Target-URI: foo://bad^host/\r\nContent-Type: text/plain\r\nContent-Length: 0\r\n\r\n\r\n\r\n"
	if _, err := io.WriteString(member, record); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"index", "--index-format", "badger", "--index-dir", filepath.Join(dir, "db"), source}
	t.Setenv("WARC_STRICT", "false")
	if err := executeCaptureCommand(t, args...); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARC_STRICT", "true")
	if err := executeCaptureCommand(t, args...); err == nil {
		t.Fatal("strict mode reused the permissive checkpoint")
	}
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte("lax-host-parsing: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := executeCaptureCommand(t, append(args, "--config", config)...); err != nil {
		t.Fatal(err)
	}
	if err := executeCaptureCommand(t, args...); err == nil {
		t.Fatal("strict mode reused the lax host checkpoint")
	}
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
	if err = executeCaptureCommand(t, "index", "--fulltext", "--index-format=badger", "--index-dir", opts.Dir, "../testdata/warc/convert.warc"); err == nil {
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
