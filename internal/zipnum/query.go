// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
)

// Reader keeps the published summary open for its lifetime. Only summary line
// offsets reside in memory; binary searches read keys from the on-disk summary.
// Published clusters must not be modified while open.
type Reader struct {
	root    string
	summary *os.File
	offsets []int64
}

func Open(root string) (*Reader, error) { return OpenContext(context.Background(), root) }

func OpenContext(ctx context.Context, root string) (r *Reader, err error) {
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	meta, e := readMeta(filepath.Join(root, metaFile))
	if e != nil {
		return nil, e
	}
	if meta.LinesPerBlock <= 0 || meta.BlocksPerPart <= 0 {
		return nil, fmt.Errorf("invalid cluster layout")
	}
	r = &Reader{root: root}
	r.summary, err = os.Open(filepath.Join(root, indexFile))
	if err != nil {
		return nil, err
	}
	summary := r.summary
	defer func() {
		if err != nil {
			err = errors.Join(err, summary.Close())
		}
	}()
	reader := bufio.NewReader(r.summary)
	var offset int64
	previous := ""
	seq := 1
	current := ""
	var end int64
	seen := map[string]bool{}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		if len(line) == 0 {
			break
		}
		if e := ctx.Err(); e != nil {
			return nil, e
		}

		b, n, e := parseIndexFields([]byte(line), true)
		if e != nil {
			return nil, e
		}
		parts := strings.Split(b.key, " ")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid summary key")
		}
		if _, e := time.Parse("20060102150405", parts[1]); e != nil {
			return nil, fmt.Errorf("invalid summary timestamp: %w", e)
		}
		if n != seq {
			return nil, fmt.Errorf("summary sequence %d, want %d", n, seq)
		}
		seq++
		if b.key < previous {
			return nil, fmt.Errorf("summary is not globally ordered")
		}
		previous = b.key
		if b.file != current {
			if seen[b.file] {
				return nil, fmt.Errorf("noncontiguous part")
			}
			seen[b.file] = true
			current = b.file
			end = 0
		}
		if b.offset != end || b.length > int64(^uint64(0)>>1)-b.offset {
			return nil, fmt.Errorf("invalid block range")
		}
		end = b.offset + b.length
		info, e := os.Lstat(filepath.Join(root, partDir, b.file))
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() || end > info.Size() {
			return nil, fmt.Errorf("invalid part file or range")
		}
		r.offsets = append(r.offsets, offset)
		offset += int64(len(line))
	}

	return r, nil
}

func (r *Reader) Close() error { return r.summary.Close() }

func (r *Reader) block(i int) (blockIndex, error) {
	br := bufio.NewReader(io.NewSectionReader(r.summary, r.offsets[i], 1<<63-1))
	line, e := br.ReadString('\n')
	if e != nil && e != io.EOF {
		return blockIndex{}, e
	}
	b, _, e := parseIndexFields([]byte(line), true)
	return b, e
}

func (r *Reader) rangeFor(lower, upper string) (int, int, error) {
	var err error
	search := func(key string) int {
		return sort.Search(len(r.offsets), func(i int) bool {
			b, e := r.block(i)
			if e != nil {
				err = e
				return true
			}
			return b.key >= key
		})
	}
	first := search(lower)
	if first > 0 {
		first--
	}
	last := len(r.offsets)
	if upper != "" {
		last = search(upper)
	}
	return first, max(first, last), err
}

func (r *Reader) Scan(ctx context.Context, lower, upper string, emit func(*index.Capture) error) error {
	_, e := r.scan(ctx, lower, upper, 0, 0, emit)
	return e
}

func (r *Reader) ScanPage(ctx context.Context, lower, upper string, page, size int, emit func(*index.Capture) error) (int, error) {
	return r.scan(ctx, lower, upper, page, size, emit)
}

func (r *Reader) scan(ctx context.Context, lower, upper string, page, size int, emit func(*index.Capture) error) (int, error) {
	first, last, e := r.rangeFor(lower, upper)
	if e != nil {
		return 0, e
	}
	pages := 1
	if size > 0 {
		pages = 0
		if last > first {
			pages = 1 + (last-first-1)/size
		}
		if emit == nil {
			return pages, nil
		}
		if page >= pages {
			return pages, nil
		}
		first += page * size
		last = min(last, first+size)
	}
	for i := first; i < last; i++ {
		if e = ctx.Err(); e != nil {
			return pages, e
		}
		b, e := r.block(i)
		if e != nil {
			return pages, e
		}
		if e = r.readBlock(ctx, b, lower, upper, emit); e != nil {
			return pages, e
		}
	}
	return pages, nil
}

func (r *Reader) readBlock(ctx context.Context, b blockIndex, lower, upper string, emit func(*index.Capture) error) (err error) {
	f, err := os.Open(filepath.Join(r.root, partDir, b.file))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	gz, err := gzip.NewReader(io.NewSectionReader(f, b.offset, b.length))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, gz.Close()) }()
	gz.Multistream(false)

	previous := ""
	first := true
	return readNormalizedLines(gz, func(row []byte) error {
		row = row[:len(row)-1]
		if e := ctx.Err(); e != nil {
			return e
		}
		line := string(row)
		if first {
			key, e := cdxIndexKey(row)
			if e != nil {
				return e
			}
			if key != b.key {
				return fmt.Errorf("block boundary disagrees with summary")
			}
			first = false
		}
		key, _, ok := strings.Cut(line, " {")
		if !ok {
			return fmt.Errorf("invalid CDXJ block entry")
		}
		if previous != "" && key < previous {
			return fmt.Errorf("unsorted block")
		}
		previous = key
		if line < lower || (upper != "" && line >= upper) {
			return nil
		}
		c, e := index.ParseLine(row)
		if e != nil {
			return e
		}
		return emit(c)
	})
}
