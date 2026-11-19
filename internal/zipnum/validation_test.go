// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddOverlapAndEmptyCluster(t *testing.T) {
	dir := t.TempDir()
	raw, base := filepath.Join(dir, "raw"), filepath.Join(dir, "base")
	writeRecords(t, raw, []string{"com,a)/ 1 {}", "com,b)/ 1 {}"}, true)
	createCluster(t, raw, base, deterministicIDs(0))
	add := filepath.Join(dir, "add")
	writeRecords(t, add, []string{"com,a)/ 1 {}", "com,b)/ 1 {}"}, true)
	out := filepath.Join(dir, "out")
	if err := (options{input: base, add: add, output: out, concurrency: 1, newPartID: deterministicIDs(10)}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := clusterRecords(t, out); len(got) != 4 {
		t.Fatalf("equal-boundary additions were lost: %q", got)
	}
	emptyRaw, empty := filepath.Join(dir, "empty-raw"), filepath.Join(dir, "empty")
	writeRecords(t, emptyRaw, nil, false)
	createCluster(t, emptyRaw, empty, deterministicIDs(20))
	emptyOut := filepath.Join(dir, "empty-out")
	if err := (options{input: empty, add: add, output: emptyOut, concurrency: 1, newPartID: deterministicIDs(30)}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := clusterRecords(t, emptyOut); len(got) != 2 {
		t.Fatalf("add to empty cluster = %q", got)
	}
}

func TestMergeSeparatedAndMixedLayouts(t *testing.T) {
	dir := t.TempDir()
	makeCluster := func(name, record string, lines, blocks int) string {
		raw := filepath.Join(dir, name+"-raw")
		cluster := filepath.Join(dir, name)
		writeRecords(t, raw, []string{record}, true)
		if err := (options{
			input: raw, output: cluster, concurrency: 1,
			linesPerBlock: lines, blocksPerPart: blocks, newPartID: deterministicIDs(len(name) * 10),
		}).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		return cluster
	}
	late := makeCluster("late", "com,z)/ 1 {}", 1, 1)
	early := makeCluster("early", "com,a)/ 1 {}", 2, 2)
	out := filepath.Join(dir, "out")
	if err := (options{input: late + "," + early, output: out, merge: true, concurrency: 1, newPartID: deterministicIDs(100)}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := clusterRecords(t, out); fmt.Sprint(got) != fmt.Sprint([]string{"com,a)/ 1 {}", "com,z)/ 1 {}"}) {
		t.Fatalf("records = %q", got)
	}
	m, err := readMeta(filepath.Join(out, metaFile))
	if err != nil {
		t.Fatal(err)
	}
	if m.LinesPerBlock != defaultLines || m.BlocksPerPart != defaultBlocks {
		t.Fatalf("mixed layout did not select defaults: %+v", m)
	}
	before := filepath.Join(dir, "before")
	if err := (options{
		input:         early + "," + late,
		output:        before,
		merge:         true,
		concurrency:   1,
		linesPerBlock: 4,
		blocksPerPart: 3,
		newPartID:     deterministicIDs(120),
	}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, _ = readMeta(filepath.Join(before, metaFile))
	if m.LinesPerBlock != 4 || m.BlocksPerPart != 3 {
		t.Fatalf("explicit merge layout = %+v", m)
	}
}

func TestReadClusterRejectsStructuralCorruption(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, root string, m clusterManifest)
	}{
		{"metadata layout", func(t *testing.T, root string, _ clusterManifest) {
			t.Helper()
			data, _ := os.ReadFile(filepath.Join(root, metaFile))
			data = bytes.Replace(data, []byte(`"lines_per_block":1`), []byte(`"lines_per_block":0`), 1)
			_ = os.WriteFile(filepath.Join(root, metaFile), data, 0o644)
		}},
		{"sequence", func(t *testing.T, root string, _ clusterManifest) {
			t.Helper()
			path := filepath.Join(root, indexFile)
			data, _ := os.ReadFile(path)
			data = bytes.Replace(data, []byte("\t1\n"), []byte("\t2\n"), 1)
			_ = os.WriteFile(path, data, 0o644)
		}},
		{"missing part", func(t *testing.T, root string, m clusterManifest) {
			t.Helper()
			_ = os.Remove(filepath.Join(root, partDir, m.parts[0].name+partExt))
		}},
		{"nonregular part", func(t *testing.T, root string, m clusterManifest) {
			t.Helper()
			path := filepath.Join(root, partDir, m.parts[0].name+partExt)
			_ = os.Remove(path)
			_ = os.Mkdir(path, 0o755)
		}},
		{"missing per-part index", func(t *testing.T, root string, m clusterManifest) {
			t.Helper()
			_ = os.Remove(filepath.Join(root, indexDir, m.parts[0].name))
		}},
		{"short per-part index", func(t *testing.T, root string, m clusterManifest) {
			t.Helper()
			_ = os.WriteFile(filepath.Join(root, indexDir, m.parts[0].name), nil, 0o644)
		}},
		{"disagreeing per-part index", func(t *testing.T, root string, m clusterManifest) {
			t.Helper()
			path := filepath.Join(root, indexDir, m.parts[0].name)
			data, _ := os.ReadFile(path)
			data = bytes.Replace(data, []byte("com,a)/ 1"), []byte("com,x)/ 1"), 1)
			_ = os.WriteFile(path, data, 0o644)
		}},
		{"bad offset", func(t *testing.T, root string, m clusterManifest) {
			t.Helper()
			for _, path := range []string{filepath.Join(root, indexFile), filepath.Join(root, indexDir, m.parts[0].name)} {
				data, _ := os.ReadFile(path)
				data = bytes.Replace(data, []byte("\t0\t"), []byte("\t1\t"), 1)
				_ = os.WriteFile(path, data, 0o644)
			}
		}},
		{"bad coverage", func(t *testing.T, root string, m clusterManifest) {
			t.Helper()
			length := fmt.Sprint(m.parts[0].blocks[0].length)
			short := fmt.Sprint(m.parts[0].blocks[0].length - 1)
			for _, path := range []string{filepath.Join(root, indexFile), filepath.Join(root, indexDir, m.parts[0].name)} {
				data, _ := os.ReadFile(path)
				data = bytes.Replace(data, []byte("\t"+length+"\t"), []byte("\t"+short+"\t"), 1)
				data = bytes.Replace(data, []byte("\t"+length+"\n"), []byte("\t"+short+"\n"), 1)
				_ = os.WriteFile(path, data, 0o644)
			}
		}},
		{"gzip checksum", func(t *testing.T, root string, m clusterManifest) {
			t.Helper()
			path := filepath.Join(root, partDir, m.parts[0].name+partExt)
			data, _ := os.ReadFile(path)
			data[len(data)-1] ^= 0xff
			_ = os.WriteFile(path, data, 0o644)
		}},
		{"indexed key", func(t *testing.T, root string, m clusterManifest) {
			t.Helper()
			for _, path := range []string{filepath.Join(root, indexFile), filepath.Join(root, indexDir, m.parts[0].name)} {
				data, _ := os.ReadFile(path)
				data = bytes.Replace(data, []byte("com,a)/ 1"), []byte("com,x)/ 1"), 1)
				_ = os.WriteFile(path, data, 0o644)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			raw, root := filepath.Join(dir, "raw"), filepath.Join(dir, "cluster")
			writeRecords(t, raw, []string{"com,a)/ 1 {}"}, true)
			createCluster(t, raw, root, deterministicIDs(0))
			m, err := readCluster(root)
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(t, root, m)
			if _, err := readCluster(root); err == nil {
				t.Fatal("corrupt cluster accepted")
			}
		})
	}
}

func TestReadClusterRejectsManifestOrdering(t *testing.T) {
	dir := t.TempDir()
	raw, root := filepath.Join(dir, "raw"), filepath.Join(dir, "cluster")
	writeRecords(t, raw, []string{"com,a)/ 1 {}", "com,b)/ 1 {}", "com,c)/ 1 {}"}, true)
	createCluster(t, raw, root, deterministicIDs(0))
	data, _ := os.ReadFile(filepath.Join(root, indexFile))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	fields2 := strings.Split(lines[1], "\t")
	fields1 := strings.Split(lines[0], "\t")
	fields2[4], fields1[4] = "1", "2"
	reordered := strings.Join(fields2, "\t") + "\n" + strings.Join(fields1, "\t") + "\n" + lines[2] + "\n"
	_ = os.WriteFile(filepath.Join(root, indexFile), []byte(reordered), 0o644)
	if _, err := readCluster(root); err == nil {
		t.Fatal("out-of-order manifest accepted")
	}
}

func TestMoreOptionPathValidation(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw")
	writeRecords(t, raw, []string{"com,a)/ 1 {}"}, true)
	dangling := filepath.Join(dir, "dangling")
	_ = os.Symlink(filepath.Join(dir, "missing"), dangling)
	tests := []options{
		{input: raw, output: dangling, concurrency: 1},
		{input: filepath.Join(dir, "none*"), output: filepath.Join(dir, "a"), concurrency: 1},
		{input: dir, output: filepath.Join(dir, "b"), concurrency: 1},
		{input: raw, add: filepath.Join(dir, "none*"), output: filepath.Join(dir, "c"), concurrency: 1},
	}
	for i, opts := range tests {
		if err := opts.run(context.Background()); err == nil {
			t.Errorf("invalid path case %d accepted", i)
		}
	}
}

func TestConnectedOverlapMergeAndMultiInputCreation(t *testing.T) {
	dir := t.TempDir()
	aRaw, bRaw := filepath.Join(dir, "a-raw"), filepath.Join(dir, "b-raw")
	writeRecords(t, aRaw, []string{"com,a)/ 1 {}", "com,c)/ 1 {}", "com,e)/ 1 {}", "com,g)/ 1 {}"}, true)
	writeRecords(t, bRaw, []string{"com,b)/ 1 {}", "com,f)/ 1 {}"}, true)
	combined := filepath.Join(dir, "combined")
	if err := (options{
		input:         aRaw + "," + bRaw,
		output:        combined,
		concurrency:   2,
		linesPerBlock: 1,
		blocksPerPart: 2,
		newPartID:     deterministicIDs(0),
	}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := clusterRecords(t, combined); len(got) != 6 {
		t.Fatalf("multi-input creation = %q", got)
	}
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for raw, cluster := range map[string]string{aRaw: a, bRaw: b} {
		if err := (options{
			input:         raw,
			output:        cluster,
			concurrency:   1,
			linesPerBlock: 1,
			blocksPerPart: 2,
			newPartID:     deterministicIDs(len(cluster) * 10),
		}).run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "merged")
	if err := (options{input: a + "," + b, output: out, merge: true, concurrency: 2, newPartID: deterministicIDs(100)}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := clusterRecords(t, out); len(got) != 6 {
		t.Fatalf("connected merge = %q", got)
	}
}

func TestAdditionalClusterManifestFailures(t *testing.T) {
	dir := t.TempDir()
	if _, err := readCluster(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing metadata accepted")
	}
	makeTwoBlock := func(t *testing.T) (string, clusterManifest) {
		t.Helper()
		raw, root := filepath.Join(t.TempDir(), "raw"), filepath.Join(t.TempDir(), "cluster")
		writeRecords(t, raw, []string{"com,a)/ 1 {}", "com,b)/ 1 {}"}, true)
		if err := (options{
			input:         raw,
			output:        root,
			concurrency:   1,
			linesPerBlock: 1,
			blocksPerPart: 2,
			newPartID:     deterministicIDs(0),
		}).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		m, err := readCluster(root)
		if err != nil {
			t.Fatal(err)
		}
		return root, m
	}
	t.Run("missing cluster index", func(t *testing.T) {
		root, _ := makeTwoBlock(t)
		_ = os.Remove(filepath.Join(root, indexFile))
		if _, err := readCluster(root); err == nil {
			t.Fatal("missing cluster index accepted")
		}
	})
	t.Run("unsorted index keys", func(t *testing.T) {
		root, _ := makeTwoBlock(t)
		path := filepath.Join(root, indexFile)
		data, _ := os.ReadFile(path)
		data = bytes.Replace(data, []byte("com,a)/ 1"), []byte("zzz"), 1)
		_ = os.WriteFile(path, data, 0o644)
		if _, err := readCluster(root); err == nil {
			t.Fatal("unsorted index keys accepted")
		}
	})
	t.Run("oversized range", func(t *testing.T) {
		root, m := makeTwoBlock(t)
		old := fmt.Sprint(m.parts[0].blocks[0].length)
		large := fmt.Sprint(m.parts[0].blocks[0].length + 100000)
		for _, path := range []string{filepath.Join(root, indexFile), filepath.Join(root, indexDir, m.parts[0].name)} {
			data, _ := os.ReadFile(path)
			data = bytes.Replace(data, []byte("\t"+old+"\t"), []byte("\t"+large+"\t"), 1)
			data = bytes.Replace(data, []byte("\t"+old+"\n"), []byte("\t"+large+"\n"), 1)
			_ = os.WriteFile(path, data, 0o644)
		}
		if _, err := readCluster(root); err == nil {
			t.Fatal("oversized range accepted")
		}
	})
	t.Run("corrupt last block", func(t *testing.T) {
		root, m := makeTwoBlock(t)
		path := filepath.Join(root, partDir, m.parts[0].name+partExt)
		data, _ := os.ReadFile(path)
		data[len(data)-1] ^= 0xff
		_ = os.WriteFile(path, data, 0o644)
		if _, err := readCluster(root); err == nil {
			t.Fatal("corrupt last block accepted")
		}
	})
}

func TestDirectErrorAndCancellationPaths(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "malformed")
	_ = os.WriteFile(malformed, []byte("bad\n"), 0o644)
	if err := rewriteIndexFile(malformed, filepath.Join(dir, "rewritten"), "part-00001.gz"); err == nil {
		t.Fatal("malformed index rewritten")
	}
	if err := rewriteIndexFile(filepath.Join(dir, "missing"), filepath.Join(dir, "x"), "part-00001.gz"); err == nil {
		t.Fatal("missing index rewritten")
	}
	if err := linkOrCopy(filepath.Join(dir, "missing"), filepath.Join(dir, "copy")); err == nil {
		t.Fatal("missing link source accepted")
	}
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	writeRecords(t, a, []string{"com,b)/ 1 {}"}, true)
	writeRecords(t, b, []string{"com,a)/ 1 {}"}, true)
	ch := make(chan fileLine, 2)
	if err := seq(context.Background(), []string{a, b}, ch); err == nil {
		t.Fatal("globally unsorted sequential files accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := seq(cancelled, []string{a}, make(chan fileLine)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, err := cdxIndexKey([]byte("key timestamp")); err == nil {
		t.Fatal("CDXJ record without payload accepted")
	}
}

func TestRunCLICreatesCluster(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in"), filepath.Join(dir, "out")
	writeRecords(t, in, []string{"com,a)/ 1 {}"}, true)
	var logs bytes.Buffer
	if err := runCLI(context.Background(), []string{"-debug", "-in", in, "-out", out, "-lines", "1", "-blocks", "1"}, &logs); err != nil {
		t.Fatal(err)
	}
	if len(clusterRecords(t, out)) != 1 || !strings.Contains(logs.String(), "Zipnum") {
		t.Fatalf("CLI output/logs invalid: %q", logs.String())
	}
}

func TestLowLevelReadAndWriteFailures(t *testing.T) {
	dir := t.TempDir()
	z := &zipnum{output: dir, concurrency: 1, linesPerBlock: 1, blocksPerPart: 1}
	if _, err := z.writePart(partition{name: "part-x", blocks: [][][]byte{{[]byte("com,a)/ 1 {}\n")}}}); err == nil {
		t.Fatal("missing parts directory accepted")
	}
	_ = os.Mkdir(filepath.Join(dir, partDir), 0o755)
	if _, err := z.writePart(partition{name: "part-y", blocks: [][][]byte{{[]byte("com,a)/ 1 {}\n")}}}); err == nil {
		t.Fatal("missing index directory accepted")
	}
	_ = os.Mkdir(filepath.Join(dir, indexDir), 0o755)
	if _, err := z.writePart(partition{name: "part-invalid", blocks: [][][]byte{{[]byte("invalid\n")}}}); err == nil {
		t.Fatal("invalid part record accepted")
	}
	_ = os.WriteFile(filepath.Join(dir, indexFile), nil, 0o644)
	if err := z.writeClusterIndex(); err == nil {
		t.Fatal("existing cluster index overwritten")
	}
	_ = os.WriteFile(filepath.Join(dir, metaFile), nil, 0o644)
	if err := writeMetadata(meta{}, filepath.Join(dir, metaFile)); err == nil {
		t.Fatal("existing metadata overwritten")
	}

	missingBlock := blockIndex{offset: 0, length: 10}
	if _, err := readTestBlock(filepath.Join(dir, "missing.gz"), missingBlock); err == nil {
		t.Fatal("missing block file accepted")
	}
	bad := filepath.Join(dir, "bad-block")
	_ = os.WriteFile(bad, []byte("bad"), 0o644)
	if _, err := readTestBlock(bad, blockIndex{length: 3}); err == nil {
		t.Fatal("invalid gzip block accepted")
	}
	writeGzip := func(path string, data string) int64 {
		f, _ := os.Create(path)
		gz := gzip.NewWriter(f)
		_, _ = gz.Write([]byte(data))
		_ = gz.Close()
		_ = f.Close()
		info, _ := os.Stat(path)
		return info.Size()
	}
	empty := filepath.Join(dir, "empty.gz")
	emptySize := writeGzip(empty, "")
	if _, err := readTestBlock(empty, blockIndex{length: emptySize}); err == nil {
		t.Fatal("empty gzip block accepted")
	}
	unsorted := filepath.Join(dir, "unsorted.gz")
	unsortedSize := writeGzip(unsorted, "com,b)/ 1 {}\ncom,a)/ 1 {}\n")
	if _, err := readTestBlock(unsorted, blockIndex{length: unsortedSize}); err == nil {
		t.Fatal("unsorted gzip block accepted")
	}
	partPath := filepath.Join(dir, partDir, "part-00000.gz")
	writeGzip(partPath, "com,b)/ 1 {}\ncom,a)/ 1 {}\n")
	if _, err := readPartRecords(sourcePart{root: dir, name: "part-00000"}); err == nil {
		t.Fatal("unsorted part accepted")
	}
}

func TestAdditionalOptionFailuresAndCancellation(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw")
	writeRecords(t, raw, []string{"com,a)/ 1 {}"}, true)
	invalidCluster := filepath.Join(dir, "invalid-cluster")
	_ = os.Mkdir(invalidCluster, 0o755)
	second := filepath.Join(dir, "second")
	writeRecords(t, second, []string{"com,b)/ 1 {}"}, true)
	parentFile := filepath.Join(dir, "parent-file")
	_ = os.WriteFile(parentFile, nil, 0o644)
	tests := []options{
		{input: raw, output: filepath.Join(dir, "repart"), repartition: true, concurrency: 1},
		{input: invalidCluster, output: filepath.Join(dir, "invalid-out"), repartition: true, concurrency: 1},
		{input: invalidCluster, add: raw + "," + second, output: filepath.Join(dir, "two-add"), concurrency: 1},
		{input: invalidCluster, add: dir, output: filepath.Join(dir, "dir-add"), concurrency: 1},
		{input: raw, output: filepath.Join(parentFile, "child"), concurrency: 1},
		{input: raw, output: filepath.Join(dir, "negative"), concurrency: 1, linesPerBlock: -1},
	}
	for i, opts := range tests {
		if err := opts.run(context.Background()); err == nil {
			t.Errorf("invalid option case %d accepted", i)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (options{input: raw, output: filepath.Join(dir, "cancelled"), concurrency: 1}).run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run = %v", err)
	}
}

func TestAddRejectsUnsortedAndInvalidGzipStreams(t *testing.T) {
	dir := t.TempDir()
	raw, base := filepath.Join(dir, "raw"), filepath.Join(dir, "base")
	writeRecords(t, raw, []string{"com,m)/ 1 {}"}, true)
	createCluster(t, raw, base, deterministicIDs(0))
	unsorted := filepath.Join(dir, "unsorted")
	writeRecords(t, unsorted, []string{"com,z)/ 1 {}", "com,a)/ 1 {}"}, true)
	if err := (options{input: base, add: unsorted, output: filepath.Join(dir, "unsorted-out"), concurrency: 1}).run(context.Background()); err == nil {
		t.Fatal("unsorted add stream accepted")
	}
	badGzip := filepath.Join(dir, "bad.gz")
	_ = os.WriteFile(badGzip, []byte("not gzip"), 0o644)
	if err := (options{input: base, add: badGzip, output: filepath.Join(dir, "gzip-out"), concurrency: 1}).run(context.Background()); err == nil {
		t.Fatal("invalid gzip add stream accepted")
	}
}

func TestFlushSyncAndCloseFailuresPropagate(t *testing.T) {
	sentinel := errors.New("injected I/O failure")
	originalFlush, originalSync, originalClose := flushBuffer, syncFile, closeIO
	t.Cleanup(func() {
		flushBuffer, syncFile, closeIO = originalFlush, originalSync, originalClose
	})

	t.Run("flush", func(t *testing.T) {
		dir := t.TempDir()
		flushBuffer = func(w *bufio.Writer) error {
			_ = w.Flush()
			return sentinel
		}
		defer func() { flushBuffer = originalFlush }()
		if err := (&zipnum{output: dir}).writeClusterIndex(); !errors.Is(err, sentinel) {
			t.Fatalf("flush error = %v", err)
		}
	})
	t.Run("sync", func(t *testing.T) {
		dir := t.TempDir()
		syncFile = func(*os.File) error { return sentinel }
		defer func() { syncFile = originalSync }()
		if err := writeMetadata(meta{}, filepath.Join(dir, "metadata")); !errors.Is(err, sentinel) {
			t.Fatalf("sync error = %v", err)
		}
	})
	t.Run("close", func(t *testing.T) {
		dir := t.TempDir()
		closeIO = func(c io.Closer) error {
			_ = c.Close()
			return sentinel
		}
		defer func() { closeIO = originalClose }()
		if err := writeMetadata(meta{}, filepath.Join(dir, "metadata")); !errors.Is(err, sentinel) {
			t.Fatalf("close error = %v", err)
		}
	})
}

func TestMetadataStrictnessAndSourceIdentityUnion(t *testing.T) {
	dir := t.TempDir()
	metadata := filepath.Join(dir, "metadata")
	for _, data := range []string{
		`{"lines_per_block":1,"blocks_per_part":1,"unknown":true}`,
		`{"lines_per_block":1,"blocks_per_part":1} {}`,
	} {
		if err := os.WriteFile(metadata, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readMeta(metadata); err == nil {
			t.Fatalf("non-strict metadata accepted: %s", data)
		}
	}
	source := filepath.Join(dir, "source")
	alias := filepath.Join(dir, "alias")
	hardlink := filepath.Join(dir, "hardlink")
	_ = os.WriteFile(source, []byte("x"), 0o644)
	_ = os.Symlink(source, alias)
	_ = os.Link(source, hardlink)
	got := unionSources([]string{source, alias, hardlink, "missing", "missing"})
	if len(got) != 2 {
		t.Fatalf("source identity union = %q", got)
	}
}

// readTestBlock exercises strict block scanning and file-open failures.
func readTestBlock(path string, block blockIndex) (recordBounds, error) {
	f, err := os.Open(path)
	if err != nil {
		return recordBounds{}, err
	}
	defer f.Close()
	return readBlockBounds(f, block, verifiedBlockBounds)
}

func TestMissingLiteralPreventsPublication(t *testing.T) {
	dir := t.TempDir()
	first, missing, out := filepath.Join(dir, "first.cdxj"), filepath.Join(dir, "absent.cdxj"), filepath.Join(dir, "out")
	writeRecords(t, first, []string{"com,a)/ 1 {}"}, false)
	err := (Options{Inputs: []string{first, missing}, Output: out, Concurrency: 1, LinesPerBlock: 1, BlocksPerPart: 1}).Run(t.Context())
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("build = %v", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published cluster: %v", err)
	}
}
