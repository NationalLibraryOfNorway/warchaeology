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

	for _, command := range []string{"dedup", "redupe"} {
		t.Run(command, func(t *testing.T) {
			out := t.TempDir()
			args := []string{command, "--output-dir", out, "--index-dir", t.TempDir(), "--min-size-gain=0", "--compress=false", "--concurrent-writers=1", source}
			if err := executeCaptureCommand(t, args...); err != nil {
				t.Fatal(err)
			}
			if command == "redupe" {
				out = filepath.Join(out, "2026")
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
