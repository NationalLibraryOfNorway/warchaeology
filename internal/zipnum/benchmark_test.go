// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
)

// benchmarkCluster creates fixtures a part at a time to keep setup memory bounded.
// ZIPNUM_BENCH_CLUSTER selects an existing immutable cluster instead; benchmarks
// never write to that directory. Fixture setup is outside the timed sub-benchmarks.
func benchmarkCluster(b *testing.B, parts, blocks, lines int) string {
	b.Helper()
	if root := os.Getenv("ZIPNUM_BENCH_CLUSTER"); root != "" {
		return root
	}
	root := filepath.Join(b.TempDir(), "cluster")
	for _, dir := range []string{filepath.Join(root, partDir), filepath.Join(root, indexDir)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
	}
	z := &zipnum{
		output:        root,
		concurrency:   1,
		linesPerBlock: lines,
		blocksPerPart: blocks,
		newPartID:     deterministicIDs(0),
	}
	for i := range parts {
		name, err := z.allocatePartName()
		if err != nil {
			b.Fatal(err)
		}
		spec := partition{name: name}
		for j := range blocks {
			var records [][]byte
			for k := range lines {
				records = append(records, []byte(fmt.Sprintf("com,example)/%08d 20200101000000 {\"offset\":%d,\"filename\":\"capture.warc.gz\"}\n", (i*blocks+j)*lines+k, k*1234)))
			}
			spec.blocks = append(spec.blocks, records)
		}
		part, err := z.writePart(spec)
		if err != nil {
			b.Fatal(err)
		}
		z.appendManifest(part)
	}
	if err := z.writeClusterIndex(); err != nil {
		b.Fatal(err)
	}
	if err := z.writeMetadata(time.Now(), nil); err != nil {
		b.Fatal(err)
	}
	return root
}

// BenchmarkSourceLoading isolates manifest loading from boundary and verification I/O.
// Boundary open counts exclude metadata and sidecar opens; use strace for all I/O.
func BenchmarkSourceLoading(b *testing.B) {
	layouts := []struct {
		name                 string
		parts, blocks, lines int
	}{
		{"many-small-parts", 1000, 1, 2},
		{"full-parts", 8, defaultBlocks, defaultLines},
	}
	if os.Getenv("ZIPNUM_BENCH_CLUSTER") != "" {
		layouts = layouts[:1]
		layouts[0].name = "existing-cluster"
	}
	for _, layout := range layouts {
		b.Run(layout.name, func(b *testing.B) {
			root := benchmarkCluster(b, layout.parts, layout.blocks, layout.lines)
			for _, mode := range []struct {
				name string
				opts options
			}{
				{"manifest", options{repartition: true}},
				{"boundaries", options{merge: true}},
				{"verified", options{merge: true, verifySources: true}},
			} {
				b.Run(mode.name, func(b *testing.B) {
					originalOpen := openPartFile
					defer func() { openPartFile = originalOpen }()
					opens := 0
					openPartFile = func(path string) (*os.File, error) { opens++; return originalOpen(path) }
					b.ReportAllocs()
					for b.Loop() {
						if _, err := mode.opts.readSourceCluster(root); err != nil {
							b.Fatal(err)
						}
					}
					b.ReportMetric(float64(opens)/float64(b.N), "boundary-opens/op")
				})
			}
		})
	}
}

type measuredReaderAt struct {
	io.ReaderAt

	bytes int64
}

func (r *measuredReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, offset)
	r.bytes += int64(n)
	return n, err
}

// BenchmarkBlockBoundaries reports actual compressed bytes read per boundary scan.
func BenchmarkBlockBoundaries(b *testing.B) {
	root := benchmarkCluster(b, 1, defaultBlocks, defaultLines)
	m, err := readClusterManifest(root)
	if err != nil {
		b.Fatal(err)
	}
	if len(m.parts) == 0 {
		b.Skip("cluster has no parts")
	}
	p := m.parts[0]
	f, err := os.Open(filepath.Join(root, partDir, p.name+partExt))
	if err != nil {
		b.Fatal(err)
	}
	defer testcheck.Close(b, f)
	for _, tc := range []struct {
		name string
		mode blockReadMode
	}{
		{"first-record", firstRecord}, {"last-record", blockBounds}, {"verified", verifiedBlockBounds},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := &measuredReaderAt{ReaderAt: f}
			block := p.blocks[0]
			if tc.mode == blockBounds {
				block = p.blocks[len(p.blocks)-1]
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := readBlockBounds(r, block, tc.mode); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(r.bytes)/float64(b.N), "compressed-B/op")
		})
	}
}

// BenchmarkClusterOperations exercises publication as well as source loading.
// Repartition rewrites the entire source; merge/add use an empty peer to isolate
// the cost of reusing unchanged parts. Outputs are removed outside measured time.
func BenchmarkClusterOperations(b *testing.B) {
	root := benchmarkCluster(b, 64, 4, 128)
	dir := b.TempDir()
	emptyRaw := filepath.Join(dir, "empty.cdxj")
	if err := os.WriteFile(emptyRaw, nil, 0o644); err != nil {
		b.Fatal(err)
	}
	m, err := readClusterManifest(root)
	if err != nil {
		b.Fatal(err)
	}
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	defer slog.SetDefault(logger)
	emptyCluster := filepath.Join(dir, "empty-cluster")
	if err := (options{
		input:         emptyRaw,
		output:        emptyCluster,
		concurrency:   1,
		linesPerBlock: m.meta.LinesPerBlock,
		blocksPerPart: m.meta.BlocksPerPart,
	}).run(context.Background()); err != nil {
		b.Fatal(err)
	}
	for _, mode := range []string{"repartition", "merge", "add"} {
		for _, verify := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/verify=%t", mode, verify), func(b *testing.B) {
				out := filepath.Join(dir, "output")
				opts := options{input: root, output: out, concurrency: 2, verifySources: verify}
				switch mode {
				case "repartition":
					opts.repartition = true
				case "merge":
					opts.merge = true
					opts.input += "," + emptyCluster
				case "add":
					opts.add = emptyRaw
				}
				b.ReportAllocs()
				for b.Loop() {
					if err := opts.run(context.Background()); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					if err := os.RemoveAll(out); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
			})
		}
	}
}
