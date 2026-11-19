// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// countingWriter measures compressed bytes before buffering, for index offsets.
type countingWriter struct {
	w     io.Writer
	count int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.count += int64(n)
	return n, err
}

// writePart writes one gzip member per block, then writes its byte-range index.
// Files are created exclusively and flushed, synced, and closed before success.
func (z *zipnum) writePart(part partition) (result outputPart, err error) {
	partPath := filepath.Join(z.output, partDir, part.name+partExt)
	indexPath := filepath.Join(z.output, indexDir, part.name)
	partFile, err := os.OpenFile(partPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return result, err
	}
	partWriter := bufio.NewWriter(partFile)
	counter := &countingWriter{w: partWriter}
	result.name = part.name
	for _, block := range part.blocks {
		offset := counter.count
		gz := gzip.NewWriter(counter)
		for _, line := range block {
			if _, err = gz.Write(line); err != nil {
				_ = gz.Close()
				_ = partFile.Close()
				return result, err
			}
		}
		if err = closeIO(gz); err != nil {
			_ = partFile.Close()
			return result, err
		}
		key, keyErr := cdxIndexKey(block[0])
		if keyErr != nil {
			_ = partFile.Close()
			return result, keyErr
		}
		result.blocks = append(result.blocks, blockIndex{key: key, file: part.name + partExt, offset: offset, length: counter.count - offset})
	}
	if len(part.blocks) > 0 {
		firstBlock := part.blocks[0]
		lastBlock := part.blocks[len(part.blocks)-1]
		result.recordBounds = recordBounds{
			first: bytes.Clone(firstBlock[0]),
			last:  bytes.Clone(lastBlock[len(lastBlock)-1]),
		}
	}
	if err = flushBuffer(partWriter); err == nil {
		err = syncFile(partFile)
	}
	if closeErr := closeIO(partFile); err == nil {
		err = closeErr
	}
	if err != nil {
		return result, err
	}
	indexFile, err := os.OpenFile(indexPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return result, err
	}
	indexWriter := bufio.NewWriter(indexFile)
	for _, b := range result.blocks {
		if _, err = fmt.Fprintf(indexWriter, partIndexFormat, b.key, b.file, b.offset, b.length); err != nil {
			break
		}
	}
	if err == nil {
		err = flushBuffer(indexWriter)
	}
	if err == nil {
		err = syncFile(indexFile)
	}
	if closeErr := closeIO(indexFile); err == nil {
		err = closeErr
	}
	return result, err
}

// writeClusterIndex writes the manifest in logical order with one-based sequences.
func (z *zipnum) writeClusterIndex() (err error) {
	f, err := os.OpenFile(filepath.Join(z.output, indexFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	seq := 0
	for _, part := range z.manifest {
		for _, b := range part.blocks {
			seq++
			if _, err = fmt.Fprintf(w, clusterIndexFormat, b.key, b.file, b.offset, b.length, seq); err != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	if err == nil {
		err = flushBuffer(w)
	}
	if err == nil {
		err = syncFile(f)
	}
	if closeErr := closeIO(f); err == nil {
		err = closeErr
	}
	return err
}

// validateOutputManifest checks completed parts without reopening output files.
// Boundary records come from the writer or the source part reused by merge/add.
func (z *zipnum) validateOutputManifest() error {
	seen := make(map[string]bool, len(z.manifest))
	var previousLast []byte
	for _, part := range z.manifest {
		if !isV7PartName(part.name) || seen[part.name] {
			return fmt.Errorf("invalid or duplicate output part %q", part.name)
		}
		seen[part.name] = true
		if len(part.blocks) == 0 || len(part.first) == 0 || len(part.last) == 0 {
			return fmt.Errorf("output part %q has no blocks or boundaries", part.name)
		}
		if bytes.Compare(part.first, part.last) > 0 ||
			(len(previousLast) > 0 && bytes.Compare(previousLast, part.first) > 0) {
			return fmt.Errorf("output parts are not globally sorted at %q", part.name)
		}
		var end int64
		var previousKey string
		for _, block := range part.blocks {
			if block.file != part.name+partExt || block.offset != end || block.length <= 0 ||
				block.length > math.MaxInt64-end {
				return fmt.Errorf("invalid output byte range for %q", part.name)
			}
			if block.key == "" || block.key < previousKey {
				return fmt.Errorf("output index keys are not sorted in %q", part.name)
			}
			end += block.length
			previousKey = block.key
		}
		key, err := cdxIndexKey(part.first)
		if err != nil || key != part.blocks[0].key {
			return fmt.Errorf("first block key for %q disagrees with its index", part.name)
		}
		previousLast = part.last
	}
	return nil
}
