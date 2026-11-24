// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"uuid"
)

func deterministicIDs(start int) func() uuid.UUID {
	n := start
	return func() uuid.UUID {
		n++
		return uuid.MustParse(fmt.Sprintf("01890f12-3456-7000-8000-%012x", n))
	}
}

func writeRecords(t *testing.T, path string, records []string, finalNewline bool) {
	t.Helper()
	data := strings.Join(records, "\n")
	if finalNewline && len(records) > 0 {
		data += "\n"
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func createCluster(t *testing.T, input, output string, gen func() uuid.UUID) {
	t.Helper()
	err := (options{
		input: input, output: output, concurrency: 2,
		linesPerBlock: 1, blocksPerPart: 1, newPartID: gen,
	}).run(context.Background())
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
}

func clusterRecords(t *testing.T, root string) []string {
	t.Helper()
	m, err := readCluster(root)
	if err != nil {
		t.Fatalf("read cluster: %v", err)
	}
	var out []string
	for _, p := range m.parts {
		records, err := readPartRecords(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			out = append(out, strings.TrimSuffix(string(record), "\n"))
		}
	}
	return out
}

func TestCreationUsesUUIDv7AndRetainsFinalRecord(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "input.cdxj")
	out := filepath.Join(dir, "cluster")
	want := []string{
		"com,example)/a 20200101000000 {\"x\":1}",
		"com,example)/b 20200101000000 {\"x\":2}",
	}
	writeRecords(t, in, want, false)
	createCluster(t, in, out, deterministicIDs(0))
	if got := clusterRecords(t, out); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("records = %q, want %q", got, want)
	}
	m, err := readCluster(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.parts {
		if !isV7PartName(p.name) {
			t.Fatalf("part name is not UUIDv7: %q", p.name)
		}
	}
}

func TestManifestOrderDoesNotFollowUUIDOrder(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "input")
	out := filepath.Join(dir, "cluster")
	want := []string{
		"com,a)/ 20200101000000 {}",
		"com,b)/ 20200101000000 {}",
	}
	writeRecords(t, in, want, true)
	ids := []uuid.UUID{
		uuid.MustParse("ffffffff-ffff-7fff-bfff-ffffffffffff"),
		uuid.MustParse("00000000-0000-7000-8000-000000000001"),
	}
	createCluster(t, in, out, func() uuid.UUID {
		id := ids[0]
		ids = ids[1:]
		return id
	})
	if got := clusterRecords(t, out); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("manifest was UUID-sorted: got %q", got)
	}
}

func TestMergeAndAddMatchSortedBaseline(t *testing.T) {
	dir := t.TempDir()
	aRaw, bRaw := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	a, b := filepath.Join(dir, "ca"), filepath.Join(dir, "cb")
	writeRecords(t, aRaw, []string{"com,a)/ 1 {}", "com,c)/ 1 {}"}, true)
	writeRecords(t, bRaw, []string{"com,b)/ 1 {}", "com,c)/ 1 {}"}, true)
	createCluster(t, aRaw, a, deterministicIDs(0))
	createCluster(t, bRaw, b, deterministicIDs(0))
	merged := filepath.Join(dir, "merged")
	if err := (options{input: a + "," + b, output: merged, merge: true, concurrency: 2, newPartID: deterministicIDs(20)}).run(context.Background()); err != nil {
		t.Fatalf("merge: %v", err)
	}
	wantMerged := []string{"com,a)/ 1 {}", "com,b)/ 1 {}", "com,c)/ 1 {}", "com,c)/ 1 {}"}
	if got := clusterRecords(t, merged); fmt.Sprint(got) != fmt.Sprint(wantMerged) {
		t.Fatalf("merged records = %q", got)
	}
	add := filepath.Join(dir, "add")
	writeRecords(t, add, []string{"com,0)/ 1 {}", "com,bb)/ 1 {}", "com,z)/ 1 {}"}, false)
	added := filepath.Join(dir, "added")
	if err := (options{input: merged, add: add, output: added, concurrency: 2, newPartID: deterministicIDs(40)}).run(context.Background()); err != nil {
		t.Fatalf("add: %v", err)
	}
	wantAdded := []string{"com,0)/ 1 {}", "com,a)/ 1 {}", "com,b)/ 1 {}", "com,bb)/ 1 {}", "com,c)/ 1 {}", "com,c)/ 1 {}", "com,z)/ 1 {}"}
	if got := clusterRecords(t, added); fmt.Sprint(got) != fmt.Sprint(wantAdded) {
		t.Fatalf("added records = %q", got)
	}
}

func TestAliasesAndExistingOutputAreSafe(t *testing.T) {
	dir := t.TempDir()
	in, alias := filepath.Join(dir, "input"), filepath.Join(dir, "alias")
	writeRecords(t, in, []string{"com,a)/ 1 {}"}, true)
	if err := os.Symlink(in, alias); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "cluster")
	createCluster(t, in+","+alias, out, deterministicIDs(0))
	if got := clusterRecords(t, out); len(got) != 1 {
		t.Fatalf("aliased input duplicated records: %q", got)
	}
	existing := filepath.Join(dir, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	err := (options{input: in, output: existing, concurrency: 1}).run(context.Background())
	if err == nil {
		t.Fatal("existing output was accepted")
	}
}

func TestRejectsUnsortedAndIDCollisions(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "input")
	writeRecords(t, in, []string{"com,b)/ 1 {}", "com,a)/ 1 {}"}, true)
	err := (options{input: in, output: filepath.Join(dir, "bad"), concurrency: 1}).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not sorted") {
		t.Fatalf("unsorted error = %v", err)
	}
	writeRecords(t, in, []string{"com,a)/ 1 {}", "com,b)/ 1 {}"}, true)
	id := uuid.MustParse("01890f12-3456-7000-8000-000000000001")
	err = (options{
		input: in, output: filepath.Join(dir, "collision"), concurrency: 1,
		linesPerBlock: 1, blocksPerPart: 1, newPartID: func() uuid.UUID { return id },
	}).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unique part name") {
		t.Fatalf("collision error = %v", err)
	}
}

// readPartRecords reads all gzip members of a part, checking record order.
func readPartRecords(p sourcePart) (records [][]byte, err error) {
	path := filepath.Join(p.root, partDir, p.name+partExt)
	var previous []byte
	err = readInputLines(path, func(line []byte) error {
		if len(previous) > 0 && bytes.Compare(previous, line) > 0 {
			return fmt.Errorf("part %q is not sorted", p.name)
		}
		previous = append(previous[:0], line...)
		records = append(records, append([]byte(nil), line...))
		return nil
	})
	return records, err
}
