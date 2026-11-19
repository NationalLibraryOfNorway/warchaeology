// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package wacz

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
)

func makePackage(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "test.wacz")
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	for name, data := range entries {
		entry, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, out.Bytes(), 0400); err != nil {
		t.Fatal(err)
	}
	return name
}

func zipped(t *testing.T, text string) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func line(key, stamp string, offset int) string {
	return fmt.Sprintf("%s %s {\"url\":\"https://example.com/\",\"filename\":\"sample.warc\",\"offset\":%d,\"length\":1}\n", key, stamp, offset)
}

func TestSeekAndMergeCaptureShards(t *testing.T) {
	for _, format := range []string{"json", "legacy"} {
		t.Run(format, func(t *testing.T) {
			const stamp = "20250101000000"
			const member = "blocks.cdx.gz"
			rows := []string{line("a", stamp, 0), line("b", stamp, 0), line("m", stamp, 1), line("m", stamp, 2), line("z", stamp, 3)}
			var blocks []byte
			var summary strings.Builder
			if format == "json" {
				summary.WriteString("!meta 0 {\"format\":\"cdxj-gzip-1.0\",\"filename\":\"" + member + "\"}\n")
			}
			for i, row := range rows {
				block := zipped(t, row)
				if i == 0 {
					block[0] = 0
				} // A narrow query must never inflate this unrelated block.
				key := capturePrefix(row)
				if format == "json" {
					fmt.Fprintf(&summary, "%s {\"offset\":%d,\"length\":%d}\n", key, len(blocks), len(block))
				} else {
					fmt.Fprintf(&summary, "%s\t%s\t%d\t%d\n", key, member, len(blocks), len(block))
				}
				blocks = append(blocks, block...)
			}
			name := makePackage(t, map[string][]byte{
				"archive/sample.warc":   []byte("abcdef"),
				"indexes/" + member:     blocks,
				"indexes/blocks.idx":    []byte(summary.String()),
				"indexes/extra.cdxj.gz": zipped(t, line("m", "20250102000000", 4)),
				"indexes/extra.cdxj":    []byte(line("m", "20250103000000", 5)),
			})
			reader, err := Open(t.Context(), name)
			if err != nil {
				t.Fatal(err)
			}
			defer testcheck.Close(t, reader)
			var offsets []int64
			err = reader.Scan(t.Context(), "m ", "m!", func(c *cdx.Capture) error {
				if c.GetSsu() != "m" || c.GetSource() != "archive/sample.warc" {
					t.Fatalf("wrong capture: %v", c)
				}
				offsets = append(offsets, c.GetOffset())
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(offsets, []int64{1, 2, 4, 5}) {
				t.Fatalf("wrong offsets %v", offsets)
			}
		})
	}
}

func TestInvalidPackagesAndCaptureRanges(t *testing.T) {
	for _, name := range []string{"missing index", "nested", "out of bounds", "unknown member"} {
		t.Run(name, func(t *testing.T) {
			entries := map[string][]byte{"archive/sample.warc": []byte("data")}
			switch name {
			case "nested":
				entries["child.wacz"] = []byte("child")
			case "out of bounds":
				entries["indexes/index.cdx"] = []byte(line("m", "20250101000000", 4))
			case "unknown member":
				entries["indexes/index.cdx"] = []byte(strings.ReplaceAll(line("m", "20250101000000", 0), "sample.warc", "missing.warc"))
			}
			reader, err := Open(t.Context(), makePackage(t, entries))
			if name == "missing index" || name == "nested" {
				if err == nil {
					_ = reader.Close()
					t.Fatal("package was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer testcheck.Close(t, reader)
			if err := reader.Scan(t.Context(), "m ", "m!", func(*cdx.Capture) error { t.Fatal("invalid row returned"); return nil }); err == nil {
				t.Fatal("bad capture was accepted")
			}
		})
	}
}
