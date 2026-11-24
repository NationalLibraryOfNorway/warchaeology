// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base32"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/julienschmidt/httprouter"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/validate"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fulltext"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/archiveapi"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/coreserver"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipnum"
	"github.com/spf13/afero"
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
		{"direct needs path", []string{"serve", "--wacz-index", "direct"}, nil, "exactly one local"},
		{"direct needs one package", []string{"serve", "--wacz-index", "direct", "a.wacz", "b.wacz"}, nil, "exactly one local"},
		{"direct rejects index format", []string{"serve", "--wacz-index", "direct", "--index-format", "badger", "a.wacz"}, nil, "index-format is not valid"},
		{"direct rejects database directory", []string{"serve", "--wacz-index", "direct", "a.wacz"}, map[string]string{"WARC_INDEX_DIR": "db"}, "index-dir is not valid"},
		{"direct rejects ingestion", []string{"serve", "--wacz-index", "direct", "--post-append", "a.wacz"}, nil, "post-append is not valid"},
		{"direct is a serving mode", []string{"index", "--wacz-index", "direct", "a.wacz"}, nil, "auto or warc"},
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

func openCommandCheckpoints(t *testing.T, command, dir string) *index.FileIndex {
	t.Helper()
	if command == "validate" {
		flags := validate.NewValidateFlags()
		options, err := flags.ToOptions()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { testcheck.Close(t, options) })
		return options.FileIndex
	}
	files, err := index.NewFileIndex(dir, true, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testcheck.Close(t, files) })
	return files
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
			want := "reserve"
			if err := executeCaptureCommand(t, args...); err == nil || !strings.Contains(err.Error(), want) {
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
			defer testcheck.Close(t, files)
			key, err := filewalker.SourceLocator(afero.NewOsFs(), source)
			if err != nil {
				t.Fatal(err)
			}
			if cached, err := files.GetFileStats(key); err != nil || cached != nil {
				t.Fatalf("file cached after failed publication: %v, %v", cached, err)
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

func TestIndexDirRoot(t *testing.T) {
	warc, err := os.ReadFile("../testdata/warc/convert.warc")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	t.Chdir(base)
	for _, year := range []string{"2024", "2025"} {
		dir := filepath.Join("warcs", year)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		data := bytes.ReplaceAll(warc, []byte("http://example.test"), []byte("http://example.test/"+year))
		if err := os.WriteFile(filepath.Join(dir, "news.warc"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	rel := filepath.Join("warcs", "2025", "news.warc")
	abs := filepath.Join(base, rel)
	root := filepath.Join(base, "warcs")
	for _, tc := range []struct{ name, root, input, want string }{
		{"both absolute", root, abs, "2025/news.warc"},
		{"relative input", root, rel, "2025/news.warc"},
		{"relative root", "warcs", abs, "2025/news.warc"},
		{"both relative", "warcs", rel, "2025/news.warc"},
		{"basename", "", abs, "news.warc"},
		{"outside", filepath.Join(root, "2024"), rel, "../2025/news.warc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"index", "--index-file", "captures.cdxj", tc.input}
			if tc.root != "" {
				args = append(args, "--dir-root", tc.root)
			}
			if err := executeCaptureCommand(t, args...); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile("captures.cdxj")
			if err != nil {
				t.Fatal(err)
			}
			for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
				c, err := cdx.ParseIngestLine([]byte(line))
				if err != nil || c.GetFilename() != tc.want {
					t.Fatalf("filename = %q, want %q, error = %v", c.GetFilename(), tc.want, err)
				}
			}
		})
	}
	if err := executeCaptureCommand(t, "index", "--recursive", "--index-format", "badger", "--index-dir", "db", "--dir-root", root, "warcs"); err != nil {
		t.Fatal(err)
	}
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: "db", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	archives := archiveapi.New(db, 100, 100)
	get := func(handler http.Handler, path string) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	for _, year := range []string{"2024", "2025"} {
		query := "?url=http://example.test/" + year + "&output=json&fl=filename&limit=1"
		var hit struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(get(cdx.CDXHandler(db), "/cdx"+query), &hit); err != nil {
			t.Fatal(err)
		}
		if hit.Filename != year+"/news.warc" {
			t.Fatalf("CDX filename = %q for year %s", hit.Filename, year)
		}
		if err := json.Unmarshal(get(archives, "/archives/cdx"+query), &hit); err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(root, year, "news.warc"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(get(archives, "/archives/"+hit.Filename), want) {
			t.Fatalf("wrong WARC bytes for year %s", year)
		}
	}
}

func TestCommandsWithoutUserDirectories(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tests Linux user directory resolution")
	}
	for _, name := range []string{"HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, "")
	}
	if err := executeCaptureCommand(t, "version"); err != nil {
		t.Fatal(err)
	}
	source := "../testdata/warc/single-record.warc"
	if err := executeCaptureCommand(t, "index", source); err != nil {
		t.Fatal(err)
	}
	if err := executeCaptureCommand(t, "index", "--index-format", "badger", "--index-dir", t.TempDir(), source); err != nil {
		t.Fatal(err)
	}
	if err := executeCaptureCommand(t, "index", "--index-format", "badger", source); err == nil || !strings.Contains(err.Error(), "--index-dir") {
		t.Fatalf("missing database directory: got %v", err)
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

func resourceBytes(id int) []byte {
	return resourceWithBody(id, strings.Repeat("payload", 500))
}

func resourceWithBody(id int, body string) []byte {
	digest := sha1.Sum([]byte(body))
	return fmt.Appendf(nil, "WARC/1.1\r\nWARC-Type: resource\r\nWARC-Record-ID: <urn:uuid:20add669-b781-4c66-977c-%012d>\r\nWARC-Date: 2022-01-01T00:00:00Z\r\nWARC-Target-URI: https://example.org/%d\r\nContent-Type: text/plain\r\nWARC-Payload-Digest: sha1:%s\r\nContent-Length: %d\r\n\r\n%s\r\n\r\n", id, id, base32.StdEncoding.EncodeToString(digest[:]), len(body), body)
}

func TestDedupOneToOneCollision(t *testing.T) {
	for _, mode := range []string{"--deterministic", "--concurrency=2"} {
		t.Run(mode, func(t *testing.T) {
			out := t.TempDir()
			args := []string{"dedup", "--one-to-one", mode, "--output-dir", out, "--index-dir", t.TempDir(), "--keep-index", "--compress=false", "--min-disk-free=0", "--min-index-disk-free=0"}
			for i := range 2 {
				p := filepath.Join(t.TempDir(), "capture.warc")
				if err := os.WriteFile(p, resourceBytes(i), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, p)
			}
			if err := executeCaptureCommand(t, args...); err == nil || !strings.Contains(err.Error(), "reserve") {
				t.Fatalf("collision error %v", err)
			}
			data, err := os.ReadFile(filepath.Join(out, "capture.warc"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, []byte("WARC-Type: resource")) || bytes.Contains(data, []byte("WARC-Type: revisit")) {
				t.Fatal("published donor lost")
			}
			if err := executeCaptureCommand(t, "audit", out); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetainedDedupSources(t *testing.T) {
	out, work := t.TempDir(), t.TempDir()
	args := []string{"dedup", "--output-dir", out, "--index-dir", work, "--keep-index", "--compress=false", "--min-disk-free=0", "--min-index-disk-free=0"}
	for i := range 2 {
		p := filepath.Join(t.TempDir(), "inputs.zip")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		z := zip.NewWriter(f)
		m, err := z.Create("archive/capture.warc")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = m.Write(resourceBytes(i)); err != nil {
			t.Fatal(err)
		}
		if err = z.Close(); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		if i == 0 && runtime.GOOS != "windows" {
			hook := filepath.Join(t.TempDir(), "skip")
			if err = os.WriteFile(hook, []byte("#!/bin/sh\nexit 10\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := executeCaptureCommand(t, append(args, "--open-input-file-hook", hook, p)...); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			if err := executeCaptureCommand(t, append(args, p)...); err != nil {
				t.Fatal(err)
			}
		}
	}
	output := filepath.Join(t.TempDir(), "output.cdxj")
	if err := executeCaptureCommand(t, "index", "--index-file", output, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte("\n")) != 2 || !bytes.Contains(data, []byte("example.org/0")) || !bytes.Contains(data, []byte("example.org/1")) {
		t.Fatalf("captures %s", data)
	}
}

func TestIndexSelectionStopsBeforeMalformedRecord(t *testing.T) {
	p := filepath.Join(t.TempDir(), "partial.warc")
	if err := os.WriteFile(p, append(resourceBytes(0), "WARC/1.1\r\ninvalid\r\n\r\n"...), 0600); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--limit=1", "--nth=1"} {
		out := filepath.Join(t.TempDir(), "index.cdxj")
		if err := executeCaptureCommand(t, "index", flag, "--post-append", "--index-file", out, p); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(out)
		if err != nil || !bytes.Contains(data, []byte("example.org/0")) {
			t.Fatalf("capture %s, %v", data, err)
		}
	}
}

func TestIndexLimitWithPairedRequest(t *testing.T) {
	for _, order := range [][2]string{{"request", "response"}, {"response", "request"}, {"request", "revisit"}, {"revisit", "request"}} {
		t.Run(strings.Join(order[:], "-"), func(t *testing.T) {
			var data bytes.Buffer
			for i, kind := range order {
				const id = "urn:uuid:20add669-b781-4c66-977c-"
				fmt.Fprintf(&data, "WARC/1.1\r\nWARC-Type: %s\r\nWARC-Record-ID: <%s%012d>\r\nWARC-Date: 2022-01-01T00:00:00Z\r\nWARC-Target-URI: https://example.org/\r\n", kind, id, i)
				if i == 1 {
					fmt.Fprintf(&data, "WARC-Concurrent-To: <%s%012d>\r\n", id, 0)
				}
				block, messageType := "HTTP/1.1 200 OK\r\n\r\nresult", "response"
				switch kind {
				case "request":
					block = "POST / HTTP/1.1\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 3\r\n\r\nx=y"
					messageType = "request"
				case "revisit":
					block = "HTTP/1.1 200 OK\r\n\r\n"
					data.WriteString("WARC-Profile: http://netpreserve.org/warc/1.1/revisit/identical-payload-digest\r\n" +
						"WARC-Payload-Digest: sha1:VL2MMHO4YXUKFWV63YHTWSBM3GXKSQ2N\r\n" +
						"WARC-Refers-To-Target-URI: https://example.org/original\r\nWARC-Refers-To-Date: 2021-01-01T00:00:00Z\r\n")
				}
				fmt.Fprintf(&data, "Content-Type: application/http; msgtype=%s\r\nContent-Length: %d\r\n\r\n%s\r\n\r\n", messageType, len(block), block)
			}
			data.WriteString("WARC/1.1\r\ninvalid\r\n\r\n")
			path := filepath.Join(t.TempDir(), "input.warc")
			if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "out.cdxj")
			if err := executeCaptureCommand(t, "index", "--post-append", "--limit=1", "--index-file", out, path); err != nil {
				t.Fatal(err)
			}
			rows, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Count(rows, []byte("\n")) != 1 || !bytes.Contains(rows, []byte("?__wb_method=post&x=y ")) || !bytes.Contains(rows, []byte(`"requestBody":"x=y"`)) {
				t.Fatalf("emitted %s", rows)
			}
		})
	}
}

const hookRejection = "test hook rejected publication"

func rejectingHook(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell hook")
	}
	path := filepath.Join(t.TempDir(), "reject-hook")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+hookRejection+"' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConversionCheckpointsFollowPublication(t *testing.T) {
	for _, mode := range []string{"disk", "shared-hook", "identity-hook"} {
		t.Run(mode, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "input.warc")
			if err := os.WriteFile(source, resourceBytes(1), 0600); err != nil {
				t.Fatal(err)
			}
			work := t.TempDir()
			args := []string{"convert", "warc", "--keep-index", "--index-dir", work, "--output-dir", t.TempDir(), "--min-disk-free=0", source}
			if mode == "identity-hook" {
				args = append(args, "--name-generator=identity")
			}
			failed := append([]string(nil), args...)
			if mode == "disk" {
				failed = append(failed, "--min-disk-free=200TB")
			} else {
				failed = append(failed, "--close-output-file-hook", rejectingHook(t))
			}
			if err := executeCaptureCommand(t, failed...); err == nil || (mode != "disk" && !strings.Contains(err.Error(), hookRejection)) {
				t.Fatalf("unexpected conversion result: %v", err)
			}
			retry := t.TempDir()
			if err := executeCaptureCommand(t, append(args, "--output-dir", retry)...); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(retry)
			if err != nil || len(entries) != 1 {
				t.Fatalf("retry outputs %v: %v", entries, err)
			}
		})
	}
}

func TestMountedConversionCheckpoints(t *testing.T) {
	inputs := make([]string, 2)
	for i := range inputs {
		inputs[i] = filepath.Join(t.TempDir(), "collection.wacz")
		var buf bytes.Buffer
		z := zip.NewWriter(&buf)
		member, err := z.Create("archive/shared.warc")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write(resourceBytes(i)); err != nil {
			t.Fatal(err)
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(inputs[i], buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range [][]string{{"convert", "warc"}, {"validate"}} {
		work := t.TempDir()
		args := append(command, "--keep-index", "--index-dir", work, "--concurrency=1")
		if command[0] == "convert" {
			args = append(args, "--output-dir", t.TempDir(), "--min-disk-free=0")
		}
		args = append(args, inputs...)
		for range 2 {
			if err := executeCaptureCommand(t, args...); err != nil {
				t.Fatal(err)
			}
		}
		db := openCommandCheckpoints(t, command[0], work)

		for _, input := range inputs {
			key, err := filewalker.MemberLocator([]string{input}, "/archive/shared.warc")
			if err != nil {
				t.Fatal(err)
			}
			result, err := db.GetFileStats(key)
			if err != nil || result == nil {
				t.Fatalf("missing checkpoint for %s: %v", key, err)
			}
		}
	}
}

func TestIncompleteHeaderPreservesPublishedIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.warc")
	out := filepath.Join(t.TempDir(), "index.cdxj")
	baseline := []byte("previous generation")
	if err := os.WriteFile(out, baseline, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(resourceBytes(0), "WARC/1.1"...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := executeCaptureCommand(t, "index", "--index-file", out, path); err == nil {
		t.Fatal("accepted truncated record")
	}
	data, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(data, baseline) {
		t.Fatalf("previous generation changed: %v", err)
	}
}

func TestTruncationPreservesBadgerGeneration(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("gzip=%t", compressed), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(t.TempDir(), "archive.warc")
			member := func(data []byte) []byte {
				if !compressed {
					return data
				}
				var b bytes.Buffer
				w := gzip.NewWriter(&b)
				if _, err := w.Write(data); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				return b.Bytes()
			}
			if compressed {
				source += ".gz"
			}
			args := []string{"index", "--index-format=badger", "--index-dir", dir, source}
			write := func(data []byte) {
				t.Helper()
				if err := os.WriteFile(source, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(member(resourceBytes(0)))
			if err := executeCaptureCommand(t, args...); err != nil {
				t.Fatal(err)
			}
			write(append(member(resourceBytes(1)), member([]byte("WARC/1.1"))...))
			if err := executeCaptureCommand(t, args...); err == nil {
				t.Fatal("published incomplete source")
			}
			db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: dir, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer testcheck.Close(t, db)
			var urls []string
			if err := db.Scan(t.Context(), "", "", func(c *cdx.Capture) error { urls = append(urls, c.GetUri()); return nil }); err != nil {
				t.Fatal(err)
			}
			if len(urls) != 1 || urls[0] != "https://example.org/0" {
				t.Fatalf("visible captures: %v", urls)
			}
		})
	}
}

func TestConversionIdentityUsesFirstRecordDate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "item.warc")
	later := bytes.ReplaceAll(resourceBytes(2), []byte("2022-01"), []byte("2023-01"))
	if err := os.WriteFile(path, append(resourceBytes(1), later...), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"--name-generator=identity", "--one-to-one"} {
		out := t.TempDir()
		args := []string{"convert", "warc", mode, "--output-dir", out, "--compress=false", "--subdir-pattern={YYYY}", "--min-disk-free=0", path}
		if err := executeCaptureCommand(t, args...); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(out, "2022", "item.warc")
		before, err := os.ReadFile(dest)
		if err != nil || !bytes.Contains(before, []byte("example.org/1")) || !bytes.Contains(before, []byte("example.org/2")) {
			t.Fatalf("identity output %s: %v", dest, err)
		}
		if err := executeCaptureCommand(t, args...); err == nil {
			t.Fatal("overwrote published WARC")
		}
		after, err := os.ReadFile(dest)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("published WARC changed")
		}
		if files, err := os.ReadDir(filepath.Dir(dest)); err != nil || len(files) != 1 {
			t.Fatalf("publication left files %v, %v", files, err)
		}
	}
}

func TestDedupIncompleteSourceRemainsUncheckpointed(t *testing.T) {
	source := filepath.Join(t.TempDir(), "input.warc")
	work := t.TempDir()
	run := func(work, out string) error {
		return executeCaptureCommand(t, "dedup", "--keep-index", "--index-dir", work, "--output-dir", out, "--one-to-one", "--compress=false", "--min-disk-free=0", "--min-index-disk-free=0", source)
	}
	if err := os.WriteFile(source, append(resourceBytes(0), "WARC/1.1"...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(work, t.TempDir()); err == nil {
		t.Fatal("accepted incomplete source")
	}
	files, err := index.NewFileIndex(work, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, files)
	key, err := filewalker.SourceLocator(afero.NewOsFs(), source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := files.GetFileStats(key)
	if err != nil || result != nil {
		t.Fatalf("checkpoint %v: %v", result, err)
	}
	if err := os.WriteFile(source, append(resourceBytes(0), resourceBytes(1)...), 0600); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := run(t.TempDir(), out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "input.warc"))
	if err != nil || !bytes.Contains(data, []byte("example.org/1")) {
		t.Fatalf("repaired capture missing: %v", err)
	}
}

func TestFullFileCoverageAndRetry(t *testing.T) {
	for _, command := range []string{"validate", "convert"} {
		t.Run(command, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "input.warc")
			work := t.TempDir()
			args := []string{command}
			if command == "convert" {
				args = append(args, "warc", "--output-dir", t.TempDir(), "--min-disk-free=0")
			}
			if err := os.WriteFile(source, append(resourceBytes(1), "WARC/1.1"...), 0600); err != nil {
				t.Fatal(err)
			}
			if err := executeCaptureCommand(t, append(args, "--limit=1", source)...); err != nil {
				t.Fatal(err)
			}
			if command == "validate" {
				if err := executeCaptureCommand(t, append(args, "--record-type=response", source)...); err == nil {
					t.Fatal("filtered scan accepted truncated input")
				}
			}
			out := t.TempDir()
			if command == "convert" {
				args = append(args, "--one-to-one", "--compress=false", "--output-dir", out)
			}
			args = append(args, "--keep-index", "--index-dir", work, source)
			if err := executeCaptureCommand(t, args...); err == nil {
				t.Fatal("accepted truncated input")
			}
			if command == "convert" {
				entries, err := os.ReadDir(out)
				if err != nil || len(entries) != 0 {
					t.Fatalf("partial output: %v, %v", entries, err)
				}
			}
			repaired := append(resourceBytes(1), resourceBytes(2)...)
			if err := os.WriteFile(source, append(repaired, "\r\n"...), 0600); err != nil {
				t.Fatal(err)
			}
			if err := executeCaptureCommand(t, args...); err != nil {
				t.Fatal(err)
			}
			if command == "convert" {
				data, err := os.ReadFile(filepath.Join(out, "input.warc"))
				if err != nil || !bytes.Contains(data, []byte("example.org/1")) || !bytes.Contains(data, []byte("example.org/2")) {
					t.Fatalf("incomplete retry output: %v", err)
				}
			}
			files := openCommandCheckpoints(t, command, work)

			result, err := files.GetFileStats(source)
			if err != nil || result == nil || result.Records() != 2 {
				t.Fatalf("checkpoint = %v, %v", result, err)
			}
		})
	}
}

func TestIdentityRetryAfterMissingInput(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	first, second := filepath.Join(dir, "one.warc"), filepath.Join(dir, "two.warc")
	if err := os.WriteFile(first, resourceBytes(1), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"convert", "warc", "--one-to-one", "--keep-index", "--compress=false", "--min-disk-free=0", "--index-dir", t.TempDir(), "--output-dir", out, first, second}
	if err := executeCaptureCommand(t, args...); err == nil {
		t.Fatal("missing input accepted")
	}
	if err := os.WriteFile(second, resourceBytes(2), 0600); err != nil {
		t.Fatal(err)
	}
	if err := executeCaptureCommand(t, args...); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(out, filepath.Base(path))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContinuePastBadArchives(t *testing.T) {
	for _, name := range []string{"validate", "convert", "dedup"} {
		t.Run(name, func(t *testing.T) {
			dir, work, out := t.TempDir(), t.TempDir(), t.TempDir()
			bad := filepath.Join(dir, "bad.zip")
			other := filepath.Join(t.TempDir(), "other.zip")
			good := filepath.Join(dir, "good.warc")
			for _, p := range []string{bad, other} {
				if err := os.WriteFile(p, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(good, resourceBytes(1), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{name}
			if name == "convert" {
				args = append(args, "warc")
			}
			if name != "validate" {
				args = append(args, "--min-disk-free=0", "--output-dir", out, "--one-to-one", "--compress=false")
			}
			err := executeCaptureCommand(t, append(args, "--keep-index", "--index-dir", work, "--continue-on-error", other, dir)...)
			if err == nil || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), other) {
				t.Fatalf("walk error = %v", err)
			}
			if name != "validate" {
				if _, err := os.Stat(filepath.Join(out, "good.warc")); err != nil {
					t.Fatal(err)
				}
				return
			}
			files := openCommandCheckpoints(t, name, work)

			r, err := files.GetFileStats(good)
			if err != nil || r == nil || r.Records() != 1 {
				t.Fatalf("good input = %v, %v", r, err)
			}
		})
	}
}

func TestServeReturnsQueuedIngestionFailure(t *testing.T) {
	dir := t.TempDir()
	for i := range 5 {
		raw := resourceBytes(i)
		if i == 0 {
			raw = append(raw, "WARC/1.1"...)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.warc", i)), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	err := executeCaptureCommand(t, "serve", "--address=127.0.0.1:0", "--concurrency=1", "--index-dir", t.TempDir(), dir)
	if err == nil || !strings.Contains(err.Error(), "incomplete archive") {
		t.Fatalf("serve = %v", err)
	}
}

func TestFulltextIndexesMixedCaseResources(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "resources.warc")
	second := bytes.Replace(resourceWithBody(2, "northern lights"), []byte("text/plain"), []byte("Text/Plain; charset=UTF-8"), 1)
	if err := os.WriteFile(source, append(resourceWithBody(1, "northern summer"), second...), 0600); err != nil {
		t.Fatal(err)
	}
	textDir := filepath.Join(dir, "text")
	if err := executeCaptureCommand(t, "index", "--index-format=badger", "--index-dir", dir, "--fulltext", "--fulltext-dir", textDir, source); err != nil {
		t.Fatal(err)
	}
	idx, err := fulltext.Open(textDir)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)
	hits, err := idx.Search(t.Context(), fulltext.Request{Query: "northern"})
	if err != nil || hits.Total != 2 {
		t.Fatalf("search = %+v, %v", hits, err)
	}
}

func TestIdentityCheckpointIsolatesHookFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell hook")
	}
	dir, work := t.TempDir(), t.TempDir()
	hook := filepath.Join(dir, "close-hook")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ncase \"$WARC_SRC_FILE_NAME\" in */bad.warc) exit 1;; esac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	good, bad := filepath.Join(dir, "good.warc"), filepath.Join(dir, "bad.warc")
	for i, path := range []string{good, bad} {
		if err := os.WriteFile(path, resourceBytes(i), 0600); err != nil {
			t.Fatal(err)
		}
	}
	err := executeCaptureCommand(t, "convert", "warc", "--one-to-one", "--keep-index", "--continue-on-error", "--concurrency=2", "--index-dir", work, "--output-dir", t.TempDir(), "--min-disk-free=0", "--close-output-file-hook", hook, good, bad)
	if err == nil {
		t.Fatal("accepted failed hook")
	}
	files, err := index.NewFileIndex(work, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, files)
	for _, path := range []string{good, bad} {
		r, err := files.GetFileStats(path)
		if err != nil || (r != nil) != (path == good) {
			t.Fatalf("%s checkpoint = %v, %v", path, r, err)
		}
	}
}

func TestIndexRejectsAmbiguousTarBeforePublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.tar")
	var data bytes.Buffer
	tw := tar.NewWriter(&data)
	for id := range 2 {
		body := resourceBytes(id)
		if err := tw.WriteHeader(&tar.Header{Name: "capture.warc", Mode: 0600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	err := executeCaptureCommand(t, "index", "--index-format", "badger", "--index-dir", dir, path)
	if err == nil || !strings.Contains(err.Error(), "capture.warc") {
		t.Fatalf("index: %v", err)
	}
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: dir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	if err = db.Scan(t.Context(), "", "", func(*cdx.Capture) error { return fmt.Errorf("capture published") }); err != nil {
		t.Fatal(err)
	}
}

func TestRepairedConversionResumes(t *testing.T) {
	out := t.TempDir()
	args := []string{"convert", "warc", "--repair", "--one-to-one", "--keep-index", "--compress=false", "--min-disk-free=0", "--output-dir", out, "--index-dir", t.TempDir(), "../testdata/warc/convert.warc"}
	if err := executeCaptureCommand(t, args...); err == nil {
		t.Fatal("expected repair diagnostics")
	}
	path := filepath.Join(out, "convert.warc")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = executeCaptureCommand(t, args...); err == nil || !strings.Contains(err.Error(), "5 errors") {
		t.Fatalf("cached diagnostics: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
		t.Fatalf("output changed on retry: %v", err)
	}
}

func TestNativeCloseHookControlsPublication(t *testing.T) {
	hook := rejectingHook(t)
	for _, ext := range []string{".warc", ".wacz"} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(t.TempDir(), "input"+ext)
			for _, step := range []struct {
				id      int
				reject  bool
				visible int
			}{{1, true, 0}, {1, false, 1}, {2, true, 1}, {2, false, 2}} {
				body := resourceBytes(step.id)
				if ext == ".wacz" {
					var data bytes.Buffer
					z := zip.NewWriter(&data)
					w, e := z.Create("archive/input.warc")
					if e != nil {
						t.Fatal(e)
					}
					if _, e = w.Write(body); e != nil {
						t.Fatal(e)
					}
					if e = z.Close(); e != nil {
						t.Fatal(e)
					}
					body = data.Bytes()
				}
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{"index", "--index-format", "badger", "--index-dir", dir, path}
				if step.reject {
					args = append(args, "--close-input-file-hook", hook)
				}
				if err := executeCaptureCommand(t, args...); (err != nil) != step.reject || (step.reject && !strings.Contains(err.Error(), hookRejection)) {
					t.Fatalf("ingest: %v", err)
				}
				db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: dir, ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				var ids []string
				err = db.Scan(t.Context(), "", "", func(c *cdx.Capture) error { ids = append(ids, c.GetRid()); return nil })
				for id := 1; id <= 2; id++ {
					rid := fmt.Sprintf("urn:uuid:20add669-b781-4c66-977c-%012d", id)
					ref, e := db.StorageRefById(t.Context(), rid)
					if e != nil || (ref != "") != (step.visible == id) {
						t.Errorf("visible=%d lookup %s: %q, %v", step.visible, rid, ref, e)
					}
				}
				if closeErr := db.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				if err != nil || len(ids) != min(1, step.visible) {
					t.Fatalf("visible=%d captures=%v: %v", step.visible, ids, err)
				}
			}
		})
	}
}

func TestDedupDiscardsIncompleteIdentity(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(t.TempDir(), "input.warc")
	run := func() error {
		return executeCaptureCommand(t, "dedup", "--one-to-one", "--compress=false", "--min-disk-free=0", "--min-index-disk-free=0", "--index-dir", t.TempDir(), "--output-dir", out, src)
	}
	if err := os.WriteFile(src, append(resourceBytes(1), "broken suffix"...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("accepted incomplete input")
	}
	files, err := os.ReadDir(out)
	if err != nil || len(files) != 0 {
		t.Fatalf("partial publication %v, %v", files, err)
	}
	if err = os.WriteFile(src, append(resourceBytes(1), resourceBytes(2)...), 0600); err != nil {
		t.Fatal(err)
	}
	if err = run(); err != nil {
		t.Fatal(err)
	}
	if err = executeCaptureCommand(t, "audit", out); err != nil {
		t.Fatal(err)
	}
}

func TestForcedCommandsReachLastMember(t *testing.T) {
	var compressed bytes.Buffer
	for i := 1; i <= 3; i++ {
		record := resourceBytes(i)
		if i == 2 {
			record = bytes.Replace(record, []byte("WARC-Type: resource"), []byte("WARC-Type: unknown"), 1)
		}
		z := gzip.NewWriter(&compressed)
		if _, err := z.Write(record); err != nil {
			t.Fatal(err)
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(t.TempDir(), "input.warc.gz")
	if err := os.WriteFile(src, compressed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"cat", "ls"} {
		for _, tc := range []struct {
			name        string
			flags       []string
			first, last bool
		}{
			{"force", []string{"--force"}, true, true},
			{"nth", []string{"--force", "--nth", "2"}, false, true},
			{"strict", nil, true, false},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				output, err := os.CreateTemp(t.TempDir(), "stdout")
				if err != nil {
					t.Fatal(err)
				}
				previous := os.Stdout
				os.Stdout = output
				defer func() { os.Stdout = previous; testcheck.Close(t, output) }()
				args := append([]string{command, "--strict", src}, tc.flags...)
				if err := executeCaptureCommand(t, args...); err != nil && tc.last {
					t.Fatal(err)
				}
				data, err := os.ReadFile(output.Name())
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(data, []byte("https://example.org/1")) != tc.first || bytes.Contains(data, []byte("https://example.org/3")) != tc.last || bytes.Contains(data, []byte("https://example.org/2")) {
					t.Fatalf("unexpected output: %s", data)
				}
			})
		}
	}
	for _, command := range []string{"convert", "validate"} {
		idx := t.TempDir()
		args := []string{command}
		out := t.TempDir()
		if command == "convert" {
			args = append(args, "warc", "--one-to-one", "--compress=false", "--output-dir", out, "--min-disk-free=0")
		}
		args = append(args, "--strict", "--force", "--keep-index", "--index-dir", idx, src)
		if err := executeCaptureCommand(t, args...); err != nil {
			t.Fatal(err)
		}
		if command == "convert" {
			data, err := os.ReadFile(filepath.Join(out, "input.warc"))
			if err != nil || !bytes.Contains(data, []byte("https://example.org/3")) {
				t.Fatalf("last record missing: %v", err)
			}
		}
		files := openCommandCheckpoints(t, command, idx)
		r, err := files.GetFileStats(src)
		testcheck.Close(t, files)
		if command == "validate" {
			if err := executeCaptureCommand(t, "validate", "--strict", "--keep-index", "--index-dir", idx, src); err == nil {
				t.Fatal("strict retry accepted malformed member")
			}
		}
		if err != nil || r == nil || r.Records() != 2 {
			t.Fatalf("%s checkpoint %v, %v", command, r, err)
		}
	}
}

func TestIndexFiltersResourceFields(t *testing.T) {
	src := filepath.Join(t.TempDir(), "input.warc")
	if err := os.WriteFile(src, resourceBytes(1), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		flag, value string
		want        bool
	}{{"--mime-type", "text/plain", true}, {"--response-code", "404", false}} {
		out := filepath.Join(t.TempDir(), "out.cdxj")
		if err := executeCaptureCommand(t, "index", tc.flag, tc.value, "--index-file", out, src); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(out)
		if err != nil || (len(data) > 0) != tc.want {
			t.Fatalf("%s output %q, %v", tc.flag, data, err)
		}
	}
}

func TestRepeatedDonorID(t *testing.T) {
	dir, out, work := t.TempDir(), t.TempDir(), t.TempDir()
	args := []string{"dedup", "--deterministic", "--compress=false", "--one-to-one", "--min-disk-free=0", "--min-size-gain=0", "--keep-index", "--index-dir", work, "--output-dir", out}
	for i, id := range []int{1, 1, 2} {
		name := filepath.Join(dir, fmt.Sprintf("%d.warc", i))
		if err := os.WriteFile(name, resourceBytes(id), 0600); err != nil {
			t.Fatal(err)
		}
		args = append(args, name)
	}
	if err := executeCaptureCommand(t, args...); err != nil {
		t.Fatal(err)
	}
	files := openCommandCheckpoints(t, "dedup", work)
	var duplicates int64
	for i := range 3 {
		key, err := filewalker.SourceLocator(afero.NewOsFs(), filepath.Join(dir, fmt.Sprintf("%d.warc", i)))
		if err != nil {
			t.Fatal(err)
		}
		stats, err := files.GetFileStats(key)
		if err != nil || stats == nil {
			t.Fatalf("checkpoint: %v", err)
		}
		duplicates += stats.Duplicates()
		data, err := os.ReadFile(filepath.Join(out, fmt.Sprintf("%d.warc", i)))
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			if !bytes.Contains(data, []byte("WARC-Type: resource")) || !bytes.Contains(data, []byte(strings.Repeat("payload", 500))) {
				t.Fatal("missing full donor copy")
			}
		} else if !bytes.Contains(data, []byte("WARC-Refers-To: <urn:uuid:20add669-b781-4c66-977c-000000000001>")) || !bytes.Contains(data, []byte("WARC-Record-ID: <urn:uuid:20add669-b781-4c66-977c-000000000002>")) {
			t.Fatal("incorrect revisit IDs")
		}
	}
	if duplicates != 1 {
		t.Fatalf("duplicate count = %d", duplicates)
	}
	if err := executeCaptureCommand(t, "audit", out); err != nil {
		t.Fatal(err)
	}
	database := t.TempDir()
	for _, conflict := range []bool{false, true} {
		if conflict {
			path := filepath.Join(out, "1.warc")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw = bytes.Replace(raw, []byte("WARC-Type:"), []byte("X-Additional: different\r\nWARC-Type:"), 1)
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := executeCaptureCommand(t, "index", "--index-format=badger", "--index-dir", database, out); err != nil {
			t.Fatal(err)
		}
		db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: database, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		core := coreserver.Handler{Index: db, Loader: &loader.Loader{StorageRefResolver: db, RecordLoader: loader.FileStorageLoader{ValidateSource: db.ValidateArchiveSource}}}
		router := httprouter.New()
		for _, route := range core.Routes() {
			router.Handler(route.Method, route.Path, route.Handler)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/record/urn:uuid:20add669-b781-4c66-977c-000000000002", nil))
		testcheck.Close(t, db)
		if conflict {
			if w.Code != 500 || !strings.Contains(w.Body.String(), "ambiguous record ID") {
				t.Fatal(w.Code, w.Body.String())
			}
		} else if w.Code != 200 || !strings.Contains(w.Body.String(), strings.Repeat("payload", 500)) {
			t.Fatal(w.Code, w.Body.String())
		}
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

func TestZipIndexCollisionPreservesOutput(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "input.zip")
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for i := range 2 {
		m, err := w.Create("same.warc")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = m.Write(resourceBytes(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "index.cdxj")
	if err := os.WriteFile(target, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := executeCaptureCommand(t, "index", "--index-file", target, name); err == nil || !strings.Contains(err.Error(), "same.warc") {
		t.Fatalf("collision: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "previous" {
		t.Fatalf("published %q, %v", data, err)
	}
}

func zipWithMembers(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	for name, body := range members {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	testcheck.Close(t, w)
	return data.Bytes()
}
func TestLiteralMountSeparatorInputs(t *testing.T) {
	dir := t.TempDir()
	outer := filepath.Join(dir, "a.zip")
	sibling := outer + "!"
	if err := os.Mkdir(sibling, 0700); err != nil {
		t.Fatal(err)
	}
	sibling = filepath.Join(sibling, "p.wacz")
	pack := func(id int) []byte {
		return zipWithMembers(t, map[string][]byte{"archive/a.warc": resourceBytes(id)})
	}
	for path, raw := range map[string][]byte{outer: zipWithMembers(t, map[string][]byte{"p.wacz": pack(1)}), sibling: pack(2)} {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, paths := range [][]string{{outer, sibling, outer}, {sibling, outer, sibling}} {
		out := filepath.Join(t.TempDir(), "rows")
		if err := executeCaptureCommand(t, append([]string{"index", "--index-file", out}, paths...)...); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(out)
		if err != nil || bytes.Count(data, []byte("\n")) != 2 {
			t.Fatal(string(data), err)
		}
	}
}
func TestWACZStagingDuplicates(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "p.wacz")
	database := filepath.Join(dir, "db")
	row := `org,example)/ 20200101000000 {"url":"https://example.org/","filename":"a.warc","offset":"0","length":"1","custom":"first"}` + "\n"
	rows := strings.Repeat(row, 257)
	write := func() {
		t.Helper()
		raw := zipWithMembers(t, map[string][]byte{"archive/a.warc": resourceBytes(1), "indexes/a.cdxj": []byte(rows)})
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	args := []string{"index", "--index-format=badger", "--index-dir", database, file}
	if err := executeCaptureCommand(t, args...); err != nil {
		t.Fatal(err)
	}
	rows += strings.Replace(row, "first", "second", 1)
	write()
	if err := executeCaptureCommand(t, args...); err == nil || !strings.Contains(err.Error(), "conflicting capture") {
		t.Fatal(err)
	}
	var unsorted bytes.Buffer
	viper.Reset()
	command := NewWarcCommand()
	command.SetOut(&unsorted)
	command.SetArgs([]string{"index", file})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Count(unsorted.String(), "first") != 257 || strings.Count(unsorted.String(), "second") != 1 {
		t.Fatal("unsorted export lost embedded rows")
	}
	out := filepath.Join(dir, "out.cdxj")
	if err := executeCaptureCommand(t, "index", "--index-file", out, file); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil || bytes.Count(data, []byte("\n")) != 2 {
		t.Fatal(err, string(data))
	}
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Dir: database, Format: "badger", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	var exported bytes.Buffer
	if err = db.Export(t.Context(), &exported); err != nil || strings.Count(exported.String(), "first") != 1 || strings.Contains(exported.String(), "second") {
		t.Fatal(err, exported.String())
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
	testcheck.Close(t, db)
	if err = executeCaptureCommand(t, "index", "--fulltext", "--index-format=badger", "--index-dir", opts.Dir, "../testdata/warc/convert.warc"); err == nil {
		t.Fatal("accepted index-only database")
	}
	db, err = cdxstore.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	if err = db.Export(t.Context(), &after); err != nil || after.String() != before.String() {
		t.Fatal(err, after.String())
	}
	stats, err := db.Stats(t.Context())
	if err != nil || stats["captures"] != int64(1) {
		t.Fatal(err, stats)
	}
}
