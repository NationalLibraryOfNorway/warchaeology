// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"path/filepath"
)

// recordBounds contains complete normalized records, including their payloads.
// Index keys alone cannot distinguish records sharing a URL key and timestamp.
type recordBounds struct {
	first []byte
	last  []byte
}

type blockReadMode int

const (
	firstRecord blockReadMode = iota
	blockBounds
	verifiedBlockBounds
)

// readPartBoundaries opens a part once and keeps only its first and last records.
// Verification consumes both boundary blocks; otherwise a multi-block part's
// first block is read only far enough to obtain its first complete record.
func readPartBoundaries(p *sourcePart, verify bool) (err error) {
	if len(p.blocks) == 0 {
		return fmt.Errorf("part %q has no blocks", p.name)
	}
	f, err := openPartFile(filepath.Join(p.root, partDir, p.name+partExt))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()

	mode := blockBounds
	if verify {
		mode = verifiedBlockBounds
	} else if len(p.blocks) > 1 {
		mode = firstRecord
	}
	bounds, err := readBlockBounds(f, p.blocks[0], mode)
	if err != nil {
		return fmt.Errorf("read first block of %q: %w", p.name, err)
	}
	if len(p.blocks) > 1 {
		mode = blockBounds
		if verify {
			mode = verifiedBlockBounds
		}
		last, err := readBlockBounds(f, p.blocks[len(p.blocks)-1], mode)
		if err != nil {
			return fmt.Errorf("read last block of %q: %w", p.name, err)
		}
		bounds.last = last.last
	}
	if verify {
		key, err := cdxIndexKey(bounds.first)
		if err != nil || key != p.blocks[0].key {
			return fmt.Errorf("first block key for %q disagrees with its index", p.name)
		}
	}
	p.recordBounds = bounds
	return nil
}

// readBlockBounds streams one indexed gzip member with constant retained memory.
// Full scans reach EOF to check the gzip checksum. firstRecord intentionally
// stops early, so it does not verify the remainder of the member.
func readBlockBounds(r io.ReaderAt, b blockIndex, mode blockReadMode) (bounds recordBounds, err error) {
	gz, err := gzip.NewReader(io.NewSectionReader(r, b.offset, b.length))
	if err != nil {
		return bounds, err
	}
	gz.Multistream(false)
	defer func() {
		if closeErr := gz.Close(); err == nil {
			err = closeErr
		}
	}()

	reader := newNormalizedLineReader(gz)
	for {
		line, readErr := reader.Next()
		if len(line) > 0 {
			if mode == verifiedBlockBounds && bounds.last != nil && bytes.Compare(bounds.last, line) > 0 {
				return bounds, fmt.Errorf("gzip block is not sorted")
			}
			if bounds.first == nil {
				bounds.first = line
			}
			// Next returns owned storage, so retaining the line requires no copy.
			bounds.last = line
		}
		if readErr != nil && readErr != io.EOF {
			return bounds, readErr
		}
		if readErr == io.EOF || mode == firstRecord {
			break
		}
	}
	if bounds.first == nil {
		return bounds, fmt.Errorf("empty gzip block")
	}
	return bounds, nil
}
