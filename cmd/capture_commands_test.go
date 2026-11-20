// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fulltext"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/archiveapi"
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
	for _, first := range []string{"request", "response"} {
		t.Run(first, func(t *testing.T) {
			var data bytes.Buffer
			second := "response"
			if first == "response" {
				second = "request"
			}
			for i, kind := range []string{first, second} {
				block := "HTTP/1.1 200 OK\r\n\r\nresult"
				if kind == "request" {
					block = "POST / HTTP/1.1\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 3\r\n\r\nx=y"
				}
				const id = "urn:uuid:20add669-b781-4c66-977c-"
				fmt.Fprintf(&data, "WARC/1.1\r\nWARC-Type: %s\r\nWARC-Record-ID: <%s%012d>\r\nWARC-Date: 2022-01-01T00:00:00Z\r\nWARC-Target-URI: https://example.org/\r\n", kind, id, i)
				if i == 1 {
					fmt.Fprintf(&data, "WARC-Concurrent-To: <%s%012d>\r\n", id, 0)
				}
				fmt.Fprintf(&data, "Content-Type: application/http; msgtype=%s\r\nContent-Length: %d\r\n\r\n%s\r\n\r\n", kind, len(block), block)
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
			if bytes.Count(rows, []byte("\n")) != 1 || !bytes.Contains(rows, []byte(`"requestBody":"x=y"`)) {
				t.Fatalf("emitted %s", rows)
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

func TestNativeCloseHookControlsPublication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell hook")
	}
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
					args = append(args, "--close-input-file-hook", "/bin/false")
				}
				if err := executeCaptureCommand(t, args...); (err != nil) != step.reject {
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
