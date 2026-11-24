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
)

// Layout records the desired number of rows per block and blocks per part.
// A zero Layout selects the defaults when writing a cluster.
type Layout struct {
	LinesPerBlock, BlocksPerPart int
}

// WriteSorted publishes a cluster from CDXJ rows in bytewise order, including
// duplicate rows. The reader supplies one record per line.
func WriteSorted(ctx context.Context, output string, source string, layout Layout, rows io.Reader) error {
	if err := checkOutputPath(output); err != nil {
		return err
	}
	z := &zipnum{
		concurrency:   1,
		linesPerBlock: layout.LinesPerBlock,
		blocksPerPart: layout.BlocksPerPart,
		getKey: func(line []byte) []byte {
			if key := getPartitionKey(line); len(key) != 0 {
				return key
			}
			key, _, _ := bytes.Cut(line, []byte(" "))
			return key
		},
	}
	if layout == (Layout{}) {
		z.linesPerBlock, z.blocksPerPart = defaultLines, defaultBlocks
	}
	if z.linesPerBlock <= 0 || z.blocksPerPart <= 0 {
		return fmt.Errorf("invalid cluster layout")
	}
	r := newNormalizedLineReader(rows)
	return writeCluster(ctx, output, z, func() ([]string, error) {
		return []string{source}, z.partitionStream(ctx, func() ([]byte, error) {
			line, err := r.Next()
			if err == io.EOF {
				return nil, nil
			}
			return line, err
		})
	})
}

// ScanRows streams the ordered CDXJ rows from the cluster summary. It validates
// each block against both indexes and retains at most one row from the prior block.
func ScanRows(ctx context.Context, root string, yield func([]byte) error) (_ Layout, err error) {
	var layout Layout
	m, err := readMeta(filepath.Join(root, metaFile))
	switch {
	case err == nil:
		if m.LinesPerBlock <= 0 || m.BlocksPerPart <= 0 {
			return layout, fmt.Errorf("invalid cluster layout")
		}
		layout = Layout{m.LinesPerBlock, m.BlocksPerPart}
	case !errors.Is(err, os.ErrNotExist):
		return layout, err
	}
	var part, index *os.File
	var idx *normalizedLineReader
	var size, end int64
	var current string
	closePart := func() error {
		if part == nil {
			return nil
		}
		closeErr := errors.Join(part.Close(), index.Close())
		part, index = nil, nil
		return closeErr
	}
	defer func() { err = errors.Join(err, closePart()) }()
	finishPart := func() error {
		if part == nil {
			return nil
		}
		if end != size {
			return fmt.Errorf("indexed ranges do not cover part %q", current)
		}
		if _, err := idx.Next(); err != io.EOF {
			return fmt.Errorf("extra or unreadable index entries in part %q", current)
		}
		return closePart()
	}
	var sequence, parts int
	var previous []byte
	err = readInputLines(filepath.Join(root, indexFile), func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		block, n, err := parseIndexFields(line, true)
		if err != nil {
			return err
		}
		sequence++
		if n != sequence {
			return fmt.Errorf("invalid summary sequence %d, want %d", n, sequence)
		}
		if block.file != current {
			if err := finishPart(); err != nil {
				return err
			}
			partPath := filepath.Join(root, partDir, block.file)
			info, err := os.Lstat(partPath)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("invalid part file %q", block.file)
			}
			index, err = os.Open(filepath.Join(root, indexDir, strings.TrimSuffix(block.file, partExt)))
			if err != nil {
				return err
			}
			part, err = os.Open(partPath)
			if err != nil {
				return errors.Join(err, index.Close())
			}
			idx = newNormalizedLineReader(index)
			current, size, end = block.file, info.Size(), 0
			parts++
		}
		entry, err := idx.Next()
		if err != nil {
			return fmt.Errorf("read per-part index for %q: %w", current, err)
		}
		indexed, _, err := parseIndexFields(entry, false)
		if err != nil || indexed != block {
			return fmt.Errorf("per-part index for %q disagrees with summary", current)
		}
		if block.offset != end || block.length > size-end {
			return fmt.Errorf("invalid block range for part %q", current)
		}
		end += block.length
		return scanRowBlock(part, block, func(row []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if bytes.Compare(previous, row) > 0 {
				return fmt.Errorf("cluster rows are out of order in part %q", current)
			}
			previous = append(previous[:0], row...)
			return yield(row)
		})
	})
	if err != nil {
		return layout, err
	}
	if err := finishPart(); err != nil {
		return layout, err
	}
	return layout, checkPartCount(ctx, filepath.Join(root, partDir), parts)
}

func scanRowBlock(f *os.File, block blockIndex, yield func([]byte) error) (err error) {
	compressed := bufio.NewReader(io.NewSectionReader(f, block.offset, block.length))
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, gz.Close()) }()
	gz.Multistream(false)
	first := true
	err = readNormalizedLines(gz, func(row []byte) error {
		if first {
			key, err := cdxIndexKey(row)
			if err != nil || key != block.key {
				return fmt.Errorf("block boundary disagrees with summary")
			}
			first = false
		}
		return yield(row)
	})
	if err != nil {
		return err
	}
	if first {
		return fmt.Errorf("empty gzip block")
	}
	if _, err := compressed.ReadByte(); err != io.EOF {
		return fmt.Errorf("indexed block does not contain exactly one gzip member")
	}
	return nil
}

func checkPartCount(ctx context.Context, path string, expected int) (err error) {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		names, err := dir.Readdirnames(128)
		for _, name := range names {
			if stem, ok := strings.CutSuffix(name, partExt); ok && validSourcePartName(stem) {
				expected--
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if expected != 0 {
		return fmt.Errorf("cluster summary and parts directory disagree")
	}
	return nil
}
