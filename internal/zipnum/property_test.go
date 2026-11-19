// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
	"uuid"
)

func TestRandomizedMergeAndAddAgainstBaseline(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	ids := func() uuid.UUID {
		var id uuid.UUID
		_, _ = rng.Read(id[:])
		id[6] = id[6]&0x0f | 0x70
		id[8] = id[8]&0x3f | 0x80
		return id
	}
	for iteration := range 5 {
		t.Run(fmt.Sprint(iteration), func(t *testing.T) {
			dir := t.TempDir()
			makeRecords := func(count int) []string {
				records := make([]string, count)
				for i := range records {
					records[i] = fmt.Sprintf("com,%c)/ 1 {\"n\":%d}", 'a'+rune(rng.Intn(8)), rng.Intn(3))
				}
				sort.Strings(records)
				return records
			}
			aRecords, bRecords, additions := makeRecords(3+rng.Intn(6)), makeRecords(3+rng.Intn(6)), makeRecords(3+rng.Intn(5))
			aRaw, bRaw, addRaw := filepath.Join(dir, "a.raw"), filepath.Join(dir, "b.raw"), filepath.Join(dir, "add.raw")
			writeRecords(t, aRaw, aRecords, rng.Intn(2) == 0)
			writeRecords(t, bRaw, bRecords, rng.Intn(2) == 0)
			writeRecords(t, addRaw, additions, rng.Intn(2) == 0)
			a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
			for raw, cluster := range map[string]string{aRaw: a, bRaw: b} {
				if err := (options{
					input: raw, output: cluster, concurrency: 2,
					linesPerBlock: 1 + rng.Intn(3), blocksPerPart: 1 + rng.Intn(3), newPartID: ids,
				}).run(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			merged := filepath.Join(dir, "merged")
			if err := (options{input: a + "," + b, output: merged, merge: true, concurrency: 2, newPartID: ids}).run(context.Background()); err != nil {
				t.Fatal(err)
			}
			mergeBaseline := append(append([]string(nil), aRecords...), bRecords...)
			sort.Strings(mergeBaseline)
			if got := clusterRecords(t, merged); fmt.Sprint(got) != fmt.Sprint(mergeBaseline) {
				t.Fatalf("merge got %q, want %q", got, mergeBaseline)
			}
			added := filepath.Join(dir, "added")
			if err := (options{input: merged, add: addRaw, output: added, concurrency: 2, newPartID: ids}).run(context.Background()); err != nil {
				t.Fatal(err)
			}
			addBaseline := append(append([]string(nil), mergeBaseline...), additions...)
			sort.Strings(addBaseline)
			if got := clusterRecords(t, added); fmt.Sprint(got) != fmt.Sprint(addBaseline) {
				t.Fatalf("add got %q, want %q", got, addBaseline)
			}
		})
	}
}
