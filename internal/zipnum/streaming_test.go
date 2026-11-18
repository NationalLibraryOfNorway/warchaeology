package zipnum

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPartitionBackpressure(t *testing.T) {
	for _, outcome := range []string{"complete", "cancel", "write error"} {
		t.Run(outcome, func(t *testing.T) {
			stage, err := createStagingDirectory(filepath.Join(t.TempDir(), "out"))
			if err != nil {
				t.Fatal(err)
			}
			z := &zipnum{output: stage, concurrency: 2, linesPerBlock: 1, blocksPerPart: 1, getKey: getPartitionKey}
			ctx, cancel := context.WithCancel(context.Background())
			blocked := make(chan struct{}, z.concurrency)
			gate := make(chan struct{})
			var release sync.Once
			writeErr := errors.New("injected write failure")
			originalSync := syncFile
			syncFile = func(f *os.File) error {
				if strings.HasSuffix(f.Name(), partExt) {
					select {
					case blocked <- struct{}{}:
					default:
					}
					<-gate
					if outcome == "write error" {
						return writeErr
					}
				}
				return originalSync(f)
			}
			var consumed atomic.Int64
			done := make(chan error, 1)
			go func() {
				done <- z.partitionStream(ctx, func() ([]byte, error) {
					n := consumed.Add(1)
					if n > 32 {
						return nil, nil
					}
					return fmt.Appendf(nil, "com,example)/%04d 1 {}\n", n), nil
				})
			}()
			finished := false
			defer func() {
				cancel()
				release.Do(func() { close(gate) })
				if !finished {
					if err := <-done; err != nil {
						t.Logf("worker cleanup: %v", err)
					}
				}
				syncFile = originalSync
			}()
			for range z.concurrency {
				select {
				case <-blocked:
				case <-time.After(5 * time.Second):
					t.Fatal("writers did not start before input EOF")
				}
			}
			// Let the producer fill the queue. Each part has one record, plus one
			// part under construction and one record waiting in partitionStream.
			time.Sleep(50 * time.Millisecond)
			if got, limit := consumed.Load(), int64(2*z.concurrency+2); got > limit {
				t.Fatalf("blocked writers allowed %d reads; limit is %d", got, limit)
			}
			if outcome == "cancel" {
				cancel()
			}
			release.Do(func() { close(gate) })
			select {
			case err = <-done:
				finished = true
			case <-time.After(5 * time.Second):
				t.Fatal("partition did not stop")
			}
			switch outcome {
			case "complete":
				if err != nil || len(z.manifest) != 32 {
					t.Fatalf("parts=%d, error=%v", len(z.manifest), err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation error: %v", err)
				}
			case "write error":
				if !errors.Is(err, writeErr) {
					t.Fatalf("write error: %v", err)
				}
			}
		})
	}
}

func TestPartitionOutOfOrderCompletion(t *testing.T) {
	stage, err := createStagingDirectory(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	z := &zipnum{output: stage, concurrency: 2, linesPerBlock: 1, blocksPerPart: 1, getKey: getPartitionKey, newPartID: deterministicIDs(0)}
	first := partPrefix + deterministicIDs(0)().String() + partExt
	last := partPrefix + deterministicIDs(15)().String()
	gate, laterFinished := make(chan struct{}), make(chan struct{})
	var release sync.Once
	originalSync := syncFile
	syncFile = func(f *os.File) error {
		if filepath.Base(f.Name()) == first {
			<-gate
		}
		err := originalSync(f)
		if filepath.Base(f.Name()) == last {
			close(laterFinished)
		}
		return err
	}
	done := make(chan error, 1)
	go func() {
		n := 0
		done <- z.partitionStream(context.Background(), func() ([]byte, error) {
			if n == 16 {
				return nil, nil
			}
			n++
			return fmt.Appendf(nil, "com,example)/%04d 1 {}\n", n), nil
		})
	}()
	finished := false
	defer func() {
		release.Do(func() { close(gate) })
		if !finished {
			if err := <-done; err != nil {
				t.Logf("worker cleanup: %v", err)
			}
		}
		syncFile = originalSync
	}()
	select {
	case <-laterFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("later parts did not finish while the first writer was blocked")
	}
	release.Do(func() { close(gate) })
	err = <-done
	finished = true
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range z.manifest {
		want := fmt.Sprintf("com,example)/%04d 1 {}\n", i+1)
		if string(p.first) != want || string(p.last) != want {
			t.Fatalf("manifest position %d: got %q, want %q", i, p.first, want)
		}
	}
}

func TestFileCursorAdvancesAndCloses(t *testing.T) {
	dir := t.TempDir()
	a, empty, b := filepath.Join(dir, "a"), filepath.Join(dir, "empty"), filepath.Join(dir, "b")
	writeRecords(t, a, []string{"com,a)/ 1 {}"}, true)
	writeRecords(t, empty, nil, true)
	writeRecords(t, b, []string{"com,b)/ 1 {}"}, false)
	c, err := newFileCursor([]string{a, empty, b})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	first := c.input.(*os.File)
	if got := string(c.Pop()); got != "com,a)/ 1 {}\n" {
		t.Fatalf("first record: %q", got)
	}
	if _, err := first.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("previous file still open: %v", err)
	}
	if got := string(c.Pop()); got != "com,b)/ 1 {}\n" || c.Err() != nil || !c.closed {
		t.Fatalf("final record=%q, error=%v, closed=%t", got, c.Err(), c.closed)
	}
	for _, paths := range [][]string{{b, a}, {a, filepath.Join(dir, "missing")}} {
		c, err := newFileCursor(paths)
		if err != nil {
			t.Fatal(err)
		}
		c.Pop()
		if c.Err() == nil || !c.closed {
			t.Fatalf("paths %v: error=%v, closed=%t", paths, c.Err(), c.closed)
		}
	}
}

func TestAddLargeRanges(t *testing.T) {
	const count = 4096
	record := func(i int) string {
		return fmt.Sprintf("com,example)/%08d 1 {\"x\":\"%s\"}", i, strings.Repeat("x", 128))
	}
	var additions, gapBase []string
	for i := range count {
		additions = append(additions, record(1024+i))
	}
	additions = append(additions, additions[len(additions)-1]) // Preserve duplicates.
	for i := range 1024 {
		gapBase = append(gapBase, record(i))
	}
	for i := range 1024 {
		gapBase = append(gapBase, record(count+1024+i))
	}
	for _, tc := range []struct {
		name string
		base []string
	}{
		{"empty", nil},
		{"prefix", []string{record(count + 2048)}},
		{"gap", gapBase},
		{"tail", []string{record(0)}},
		{"overlap", []string{record(0), record(count + 2048)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			baseRaw, addRaw := filepath.Join(dir, "base.cdxj"), filepath.Join(dir, "add.cdxj")
			writeRecords(t, baseRaw, tc.base, true)
			writeRecords(t, addRaw, additions, false)
			base, out := filepath.Join(dir, "base"), filepath.Join(dir, "out")
			if err := (options{input: baseRaw, output: base, concurrency: 2, linesPerBlock: 128, blocksPerPart: 8}).run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := (options{input: base, add: addRaw, output: out, concurrency: 2}).run(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := append(append([]string(nil), tc.base...), additions...)
			sort.Strings(want)
			if got := clusterRecords(t, out); !reflect.DeepEqual(got, want) {
				t.Fatalf("merged records differ: got %d, want %d", len(got), len(want))
			}
		})
	}
}

func TestAddWritesBeforeInputEOF(t *testing.T) {
	record := func(i int) string {
		return fmt.Sprintf("com,example)/%08d 1 {\"x\":\"%s\"}", i, strings.Repeat("x", 512))
	}
	var additions, gapBase []string
	for i := range 4096 {
		additions = append(additions, record(1000+i))
	}
	for _, start := range []int{0, 9000} {
		for i := range 64 {
			gapBase = append(gapBase, record(start+i))
		}
	}
	for _, tc := range []struct {
		name string
		base []string
	}{
		{"empty", nil},
		{"prefix", []string{record(9000)}},
		{"gap", gapBase},
		{"tail", []string{record(0)}},
		{"overlap", []string{record(0), record(9000)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw, add := filepath.Join(dir, "base.cdxj"), filepath.Join(dir, "add.cdxj")
			writeRecords(t, raw, tc.base, true)
			writeRecords(t, add, additions, true)
			base := filepath.Join(dir, "base")
			if err := (options{input: raw, output: base, concurrency: 2, linesPerBlock: 16, blocksPerPart: 4}).run(context.Background()); err != nil {
				t.Fatal(err)
			}
			manifest, err := readCluster(base)
			if err != nil {
				t.Fatal(err)
			}
			cursor, err := newRawCursor(add)
			if err != nil {
				t.Fatal(err)
			}
			defer cursor.Close()
			input := cursor.input.(*os.File)
			info, err := input.Stat()
			if err != nil {
				t.Fatal(err)
			}
			stage, err := createStagingDirectory(filepath.Join(dir, "out"))
			if err != nil {
				t.Fatal(err)
			}
			var firstWrite sync.Once
			var position int64
			var positionErr error
			originalSync := syncFile
			syncFile = func(f *os.File) error {
				firstWrite.Do(func() { position, positionErr = input.Seek(0, io.SeekCurrent) })
				return originalSync(f)
			}
			defer func() { syncFile = originalSync }()
			z := &zipnum{output: stage, concurrency: 2, linesPerBlock: 16, blocksPerPart: 4, getKey: getPartitionKey}
			if err := z.addToCluster(context.Background(), manifest, cursor); err != nil {
				t.Fatal(err)
			}
			if positionErr != nil || position <= 0 || position >= info.Size() {
				t.Fatalf("first write followed reading %d of %d bytes: %v", position, info.Size(), positionErr)
			}
		})
	}
}

func TestMergeLongOverlapChain(t *testing.T) {
	dir := t.TempDir()
	var want []string
	var clusters []string
	for peer := range 2 {
		var records []string
		for i := range 16384 {
			// Shift the second cluster by half a part so overlaps connect
			// all the way through both clusters, rather than one pair at a time.
			records = append(records, fmt.Sprintf("com,example)/%08d 1 {}", i+peer*512))
		}
		want = append(want, records...)
		raw := filepath.Join(dir, fmt.Sprintf("input-%d", peer))
		cluster := filepath.Join(dir, fmt.Sprintf("cluster-%d", peer))
		writeRecords(t, raw, records, true)
		if err := (options{input: raw, output: cluster, concurrency: 2, linesPerBlock: 128, blocksPerPart: 8}).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		clusters = append(clusters, cluster)
	}
	out := filepath.Join(dir, "merged")
	if err := (options{input: strings.Join(clusters, ","), merge: true, output: out, concurrency: 2}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	sort.Strings(want)
	if got := clusterRecords(t, out); !reflect.DeepEqual(got, want) {
		t.Fatalf("overlap chain differs: got %d records, want %d", len(got), len(want))
	}
}

func TestLateInputErrorRemovesStreamingOutput(t *testing.T) {
	dir := t.TempDir()
	var records []string
	for i := range 4096 {
		records = append(records, fmt.Sprintf("com,example)/%08d 1 {}", i))
	}
	records = append(records, records[0])
	raw := filepath.Join(dir, "unsorted.cdxj")
	writeRecords(t, raw, records, true)
	var written atomic.Int64
	originalSync := syncFile
	syncFile = func(f *os.File) error {
		if strings.HasSuffix(f.Name(), partExt) {
			written.Add(1)
		}
		return originalSync(f)
	}
	defer func() { syncFile = originalSync }()
	out := filepath.Join(dir, "out")
	err := (options{input: raw, output: out, concurrency: 2, linesPerBlock: 16, blocksPerPart: 4}).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not sorted") {
		t.Fatalf("late input error: %v", err)
	}
	if written.Load() == 0 {
		t.Fatal("test did not reach streaming writes before the input error")
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output published after failure: %v", err)
	}
	stages, err := filepath.Glob(filepath.Join(dir, ".out.tmp-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("staging directories remain: %v, error=%v", stages, err)
	}
}
