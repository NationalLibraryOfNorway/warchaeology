// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base32"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nationallibraryofnorway/gowarc/v4"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/redupe"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcdigest"
)

func TestHTTPRevisitFramingAndTrailers(t *testing.T) {
	// The short entity saves space only when its stored chunk framing is counted.
	a, b := strings.Repeat("abc", 1000), strings.Repeat("def", 32)
	captures := []struct {
		entity  string
		chunked bool
		trailer string
		donor   int // one-based input record number, zero for full records
	}{
		{a, true, "first", 0},
		{a, true, "second", 0},
		{a, false, "", 1},
		{b, false, "", 0},
		{b, true, "", 4},
	}
	id := func(n int) string { return fmt.Sprintf("urn:uuid:20add669-b781-4c66-977c-%012d", n) }
	var input bytes.Buffer
	for i, c := range captures {
		header := fmt.Sprintf("HTTP/1.1 200 OK\r\nX-Capture: %d\r\n", i+1)
		body := c.entity
		if c.chunked {
			header += "Transfer-Encoding: chunked\r\n"
			var chunks strings.Builder
			for j := 0; j < len(body); j += 3 {
				chunk := body[j:min(j+3, len(body))]
				fmt.Fprintf(&chunks, "%x\r\n%s\r\n", len(chunk), chunk)
			}
			body = chunks.String() + "0\r\n"
			if c.trailer != "" {
				header += "Trailer: X-End\r\n"
				body += "X-End: " + c.trailer + "\r\n"
			}
			body += "\r\n"
		} else {
			header += fmt.Sprintf("Content-Length: %d\r\n", len(body))
		}
		block := header + "\r\n" + body
		sum := sha1.Sum([]byte(c.entity))
		fmt.Fprintf(&input, "WARC/1.1\r\nWARC-Type: response\r\nWARC-Record-ID: <%s>\r\nWARC-Target-URI: https://example.org/%d\r\nWARC-Date: 2026-01-0%dT00:00:00Z\r\nWARC-Payload-Digest: sha1:%s\r\nContent-Type: application/http; msgtype=response\r\nContent-Length: %d\r\n\r\n%s\r\n\r\n", id(i+1), i+1, i+1, base32.StdEncoding.EncodeToString(sum[:]), len(block), block)
	}
	source := filepath.Join(t.TempDir(), "input.warc")
	if err := os.WriteFile(source, input.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}

	reportFile := filepath.Join(t.TempDir(), "audit.json")
	if err := executeCaptureCommand(t, "redupe", "--audit", "--report", reportFile, "--index-dir", t.TempDir(), "--min-size-gain=0", source); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(reportFile)
	if err != nil {
		t.Fatal(err)
	}
	var report redupe.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if p := report.Periods["2026"]; p == nil || p.Full != 3 || p.Revisits != 2 {
		t.Fatalf("audit plan: %+v", p)
	}

	var cleanManifest []map[string]any
	for _, mode := range []string{"dedup", "redupe", "inventory", "verified-plan"} {
		command := mode
		if mode == "inventory" || mode == "verified-plan" {
			command = "redupe"
		}
		t.Run(mode, func(t *testing.T) {
			out := t.TempDir()
			work := t.TempDir()
			args := []string{command, "--output-dir", out, "--index-dir", work, "--min-size-gain=0", "--compress=false", "--concurrent-writers=1", source}
			if mode != command {
				reportFile := filepath.Join(t.TempDir(), "preview.json")
				preview := append(append([]string{}, args...), "--audit", "--report", reportFile, "--output-dir", t.TempDir())
				if mode == "verified-plan" {
					preview = append(preview, "--verify-payloads")
				}
				if err := executeCaptureCommand(t, preview...); err != nil {
					t.Fatal(err)
				}
				readReport := func() redupe.Report {
					data, err := os.ReadFile(reportFile)
					if err != nil {
						t.Fatal(err)
					}
					var r redupe.Report
					if err := json.Unmarshal(data, &r); err != nil {
						t.Fatal(err)
					}
					return r
				}
				if r := readReport(); r.RetainedType != mode || r.RetainedBytes <= 0 {
					t.Fatalf("retention: %+v", r)
				}
				reportFile = filepath.Join(t.TempDir(), "execution.json")
				args = append(args, "--report", reportFile)
				if err := executeCaptureCommand(t, args...); err != nil {
					t.Fatal(err)
				}
				phases := []string{"inventory", "sort"}
				if mode == "verified-plan" {
					phases = append(phases, "verified planning")
				}
				if r := readReport(); !reflect.DeepEqual(r.Reused, phases) {
					t.Fatalf("reuse: %+v", r)
				}
			} else if err := executeCaptureCommand(t, args...); err != nil {
				t.Fatal(err)
			}
			if command == "redupe" {
				out = filepath.Join(out, "2026")
				raw, err := os.ReadFile(filepath.Join(out, "manifest.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				var entries []map[string]any
				for line := range strings.Lines(string(raw)) {
					var entry map[string]any
					if err := json.Unmarshal([]byte(line), &entry); err != nil {
						t.Fatal(err)
					}
					// Generated warcinfo changes physical locations. Check each
					// mapping in its own output before comparing provenance.
					loc := entry["output"].(map[string]any)
					data, err := os.ReadFile(filepath.Join(out, loc["file"].(string)))
					if err != nil {
						t.Fatal(err)
					}
					offset := int(loc["offset"].(float64))
					if offset < 0 || offset >= len(data) {
						t.Fatal(loc)
					}
					header, _, _ := bytes.Cut(data[offset:], []byte("\r\n\r\n"))
					if !bytes.HasPrefix(header, []byte("WARC/")) || !bytes.Contains(header, []byte(fmt.Sprintf("WARC-Record-ID: <%s>", entry["recordId"]))) {
						t.Fatal("invalid output mapping", entry)
					}
					delete(entry, "output")
					entries = append(entries, entry)
				}
				if mode == "redupe" {
					cleanManifest = entries
				} else if !reflect.DeepEqual(cleanManifest, entries) {
					t.Fatal("manifest differs from clean rewrite", entries)
				}
			}
			dir := t.TempDir()
			if err := executeCaptureCommand(t, "index", "--index-format=badger", "--index-dir", dir, out); err != nil {
				t.Fatal(err)
			}
			db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: dir, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer testcheck.Close(t, db)
			replay := loader.Loader{StorageRefResolver: db, RecordLoader: loader.FileStorageLoader{}}
			for i, c := range captures {
				replay.NoUnpack = true
				record, err := replay.RecordByID(t.Context(), id(i+1))
				if err != nil || record == nil {
					t.Fatalf("capture %d: %v", i+1, err)
				}
				if c.donor != 0 {
					if record.Type() != gowarc.Revisit || record.WarcHeader().GetId(gowarc.WarcRefersTo) != id(c.donor) {
						t.Fatalf("capture %d: unexpected revisit target: %v", i+1, record.WarcHeader())
					}
					testcheck.Close(t, record)
					replay.NoUnpack = false
					record, err = replay.RecordByID(t.Context(), id(i+1))
					if err != nil {
						t.Fatal(err)
					}
				}
				if record.Type() != gowarc.Response {
					t.Fatalf("capture %d: %s", i+1, record.Type())
				}
				raw, err := record.Block().RawBytes()
				if err != nil {
					t.Fatal(err)
				}
				message, err := io.ReadAll(raw)
				if err != nil {
					t.Fatal(err)
				}
				h := record.WarcHeader()
				if c.donor != 0 {
					digest := h.Get(gowarc.WarcBlockDigest)
					if digest == "" {
						t.Fatal("missing reconstructed block digest")
					}
					if err := warcdigest.Verify(digest, bytes.NewReader(message)); err != nil {
						t.Fatal(err)
					}
					if n, err := record.ContentLength(); err != nil || n != int64(len(message)) {
						t.Fatalf("reconstructed length %d: %v", n, err)
					}
				}
				resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(message)), nil)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				testcheck.Close(t, resp.Body)
				if err != nil || string(body) != c.entity || resp.Header.Get("X-Capture") != fmt.Sprint(i+1) || resp.Trailer.Get("X-End") != c.trailer || (len(resp.TransferEncoding) > 0) != c.chunked {
					t.Fatalf("capture %d: HTTP body, headers, framing, or trailers changed: %v", i+1, err)
				}
				digest := h.Get(gowarc.WarcPayloadDigest)
				if digest == "" {
					t.Fatal("missing payload digest")
				}
				if err := warcdigest.Verify(digest, bytes.NewReader(body)); err != nil {
					t.Fatal(err)
				}
				testcheck.Close(t, record)
			}
		})
	}
}
