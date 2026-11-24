// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCLIValidationAndVersion(t *testing.T) {
	var out bytes.Buffer
	if err := runCLI(context.Background(), []string{"-version"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Version:") {
		t.Fatalf("version output = %q", out.String())
	}
	if code := realMain([]string{"-version"}, &out); code != 0 {
		t.Fatalf("version exit code = %d", code)
	}
	if code := realMain([]string{"-unknown"}, &out); code != 1 {
		t.Fatalf("error exit code = %d", code)
	}
	if err := runCLI(context.Background(), []string{"-unknown"}, &out); err == nil {
		t.Fatal("unknown flag accepted")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	writeRecords(t, in, []string{"com,a)/ 1 {}"}, true)
	tests := []struct {
		name string
		opts options
	}{
		{"modes", options{input: in, output: filepath.Join(dir, "a"), merge: true, repartition: true, concurrency: 1}},
		{"concurrency", options{input: in, output: filepath.Join(dir, "b"), concurrency: 0}},
		{"lines", options{input: in, output: filepath.Join(dir, "c"), concurrency: 1, linesSet: true}},
		{"blocks", options{input: in, output: filepath.Join(dir, "d"), concurrency: 1, blocksSet: true}},
		{"empty output", options{input: in, concurrency: 1}},
		{"merge count", options{input: in, output: filepath.Join(dir, "e"), merge: true, concurrency: 1}},
		{"repartition count", options{
			input:       in + "," + filepath.Join(dir, "missing*"),
			output:      filepath.Join(dir, "f"),
			repartition: true,
			concurrency: 1,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.opts.run(context.Background()); err == nil {
				t.Fatal("invalid options accepted")
			}
		})
	}
}

func TestRepartitionUsesManifestOrderAndDefaultLayout(t *testing.T) {
	dir := t.TempDir()
	in, cluster := filepath.Join(dir, "in"), filepath.Join(dir, "cluster")
	want := []string{"com,a)/ 1 {}", "com,b)/ 1 {}"}
	writeRecords(t, in, want, true)
	createCluster(t, in, cluster, deterministicIDs(0))
	out := filepath.Join(dir, "repartitioned")
	if err := (options{input: cluster, output: out, repartition: true, concurrency: 2, newPartID: deterministicIDs(10)}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := clusterRecords(t, out); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("records = %q", got)
	}
	m, err := readMeta(filepath.Join(out, metaFile))
	if err != nil {
		t.Fatal(err)
	}
	if m.LinesPerBlock != defaultLines || m.BlocksPerPart != defaultBlocks {
		t.Fatalf("default layout = %d/%d", m.LinesPerBlock, m.BlocksPerPart)
	}
}

func TestLegacyMigrationAndUUIDReuse(t *testing.T) {
	dir := t.TempDir()
	in, source := filepath.Join(dir, "in"), filepath.Join(dir, "source")
	writeRecords(t, in, []string{"com,a)/ 1 {}"}, true)
	createCluster(t, in, source, deterministicIDs(0))
	emptyRaw, empty := filepath.Join(dir, "empty"), filepath.Join(dir, "empty-cluster")
	writeRecords(t, emptyRaw, nil, false)
	createCluster(t, emptyRaw, empty, deterministicIDs(50))
	srcManifest, err := readCluster(source)
	if err != nil {
		t.Fatal(err)
	}
	srcPart := srcManifest.parts[0]
	preservedOut := filepath.Join(dir, "preserved")
	if err := (options{
		input:       source + "," + empty,
		output:      preservedOut,
		merge:       true,
		concurrency: 1,
		newPartID:   deterministicIDs(60),
	}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	preserved, err := readCluster(preservedOut)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{filepath.Join(source, partDir, srcPart.name+partExt), filepath.Join(preservedOut, partDir, preserved.parts[0].name+partExt)},
		{filepath.Join(source, indexDir, srcPart.name), filepath.Join(preservedOut, indexDir, preserved.parts[0].name)},
	} {
		a, _ := os.Stat(pair[0])
		b, _ := os.Stat(pair[1])
		if !os.SameFile(a, b) {
			t.Fatalf("unchanged UUID part was not hardline: %v", pair)
		}
	}

	legacy := filepath.Join(dir, "legacy")
	if err := (options{
		input:         source,
		output:        legacy,
		repartition:   true,
		concurrency:   1,
		linesPerBlock: 1,
		blocksPerPart: 1,
		newPartID:     deterministicIDs(70),
	}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	lm, _ := readCluster(legacy)
	old := lm.parts[0].name
	if err := os.Rename(filepath.Join(legacy, partDir, old+partExt), filepath.Join(legacy, partDir, "part-00000.gz")); err != nil {
		t.Fatal(err)
	}
	idxData, _ := os.ReadFile(filepath.Join(legacy, indexDir, old))
	idxData = bytes.ReplaceAll(idxData, []byte(old+partExt), []byte("part-00000.gz"))
	if err := os.WriteFile(filepath.Join(legacy, indexDir, "part-00000"), idxData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(legacy, indexDir, old)); err != nil {
		t.Fatal(err)
	}
	clusterData, _ := os.ReadFile(filepath.Join(legacy, indexFile))
	clusterData = bytes.ReplaceAll(clusterData, []byte(old+partExt), []byte("part-00000.gz"))
	if err := os.WriteFile(filepath.Join(legacy, indexFile), clusterData, 0o644); err != nil {
		t.Fatal(err)
	}
	migrated := filepath.Join(dir, "migrated")
	if err := (options{
		input:       legacy + "," + empty,
		output:      migrated,
		merge:       true,
		concurrency: 1,
		newPartID:   deterministicIDs(80),
	}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	mm, _ := readCluster(migrated)
	if !isV7PartName(mm.parts[0].name) {
		t.Fatalf("legacy name not migrated: %q", mm.parts[0].name)
	}
	legacyData, _ := os.Stat(filepath.Join(legacy, partDir, "part-00000.gz"))
	migratedData, _ := os.Stat(filepath.Join(migrated, partDir, mm.parts[0].name+partExt))
	if !os.SameFile(legacyData, migratedData) {
		t.Fatal("legacy gzip data was not hardline")
	}
}

func TestGzipMembersAndCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []string{"com,a)/ 1 {}\n", "com,b)/ 1 {}"} {
		gz := gzip.NewWriter(f)
		_, _ = gz.Write([]byte(record))
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	out := filepath.Join(dir, "cluster")
	createCluster(t, path, out, deterministicIDs(0))
	if got := clusterRecords(t, out); len(got) != 2 {
		t.Fatalf("concatenated members produced %q", got)
	}
	bad := filepath.Join(dir, "bad.gz")
	if err := os.WriteFile(bad, []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (options{input: bad, output: filepath.Join(dir, "bad-out"), concurrency: 1}).run(context.Background()); err == nil {
		t.Fatal("invalid gzip accepted")
	}
}

func TestIndexParserRejectsMalformedEntries(t *testing.T) {
	tests := []string{
		"few\tfields\n",
		"\tpart-00000.gz\t0\t1\t1\n",
		"key\t../part-00000.gz\t0\t1\t1\n",
		"key\tpart-nope.gz\t0\t1\t1\n",
		"key\tpart-00000.gz\tbad\t1\t1\n",
		"key\tpart-00000.gz\t-1\t1\t1\n",
		"key\tpart-00000.gz\t0\t0\t1\n",
		"key\tpart-00000.gz\t0\t1\tbad\n",
	}
	for _, line := range tests {
		if _, _, err := parseIndexFields([]byte(line), true); err == nil {
			t.Errorf("accepted malformed entry %q", line)
		}
	}
	if !validSourcePartName("part-12") || validSourcePartName("else") || validSourcePartName("part-") {
		t.Fatal("legacy filename validation is wrong")
	}
}

func TestLinkFallbackAndExclusiveDestination(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}
	original := linkFile
	linkFile = func(string, string) error { return syscall.EXDEV }
	t.Cleanup(func() { linkFile = original })
	if err := linkOrCopy(src, dst); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dst); string(data) != "data" {
		t.Fatalf("copied data = %q", data)
	}
	if err := linkOrCopy(src, dst); err == nil {
		t.Fatal("existing destination overwritten")
	}
	linkFile = func(string, string) error { return errors.New("fatal") }
	if err := linkOrCopy(src, filepath.Join(dir, "other")); err == nil {
		t.Fatal("unexpected hardlink error ignored")
	}
}

func TestSequentialProducerAndHelpers(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	writeRecords(t, a, []string{"com,a)/ 1 {}"}, false)
	writeRecords(t, b, []string{"com,b)/ 1 {}"}, true)
	ch := make(chan fileLine, 2)
	if err := seq(context.Background(), []string{a, b}, ch); err != nil {
		t.Fatal(err)
	}
	if len(ch) != 2 {
		t.Fatalf("seq emitted %d records", len(ch))
	}
	if _, err := cdxIndexKey([]byte("invalid")); err == nil {
		t.Fatal("invalid CDXJ key accepted")
	}
	for _, line := range [][]byte{nil, []byte("x"), []byte("abcd"), []byte("plain key payload"), []byte("mailto:x 1 {}")} {
		_ = getPartitionKey(line)
		_ = lineFilter(line)
	}
}
