// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"bytes"
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// partition is an ordered list of blocks, each containing complete CDXJ records.
type partition struct {
	blocks [][][]byte
	name   string
	index  int
}

// partition keeps at most concurrency queued parts and concurrency active writers.
// Manifest positions are assigned here, so completion order cannot change record order.
func (z *zipnum) partition(ctx context.Context, fileLines <-chan fileLine) error {
	parts := make(chan partition, z.concurrency)
	g, ctx := errgroup.WithContext(ctx)
	for range z.concurrency {
		g.Go(func() error {
			for part := range parts {
				if err := ctx.Err(); err != nil {
					return err
				}
				result, err := z.writePart(part)
				if err != nil {
					return err
				}
				z.mu.Lock()
				z.manifest[part.index] = result
				z.mu.Unlock()
			}
			return nil
		})
	}
	g.Go(func() error {
		defer close(parts)
		return z.splitParts(ctx, fileLines, parts)
	})
	return g.Wait()
}

// splitParts checks ordering and sends each completed part before reading further.
func (z *zipnum) splitParts(ctx context.Context, fileLines <-chan fileLine, parts chan<- partition) error {
	var currentKey, previous []byte
	var block [][]byte
	var blocks [][][]byte
	flush := func() error {
		if len(block) > 0 {
			blocks = append(blocks, block)
			block = nil
		}
		if len(blocks) == 0 {
			return nil
		}
		name, err := z.allocatePartName()
		if err != nil {
			return err
		}
		z.mu.Lock()
		index := len(z.manifest)
		z.manifest = append(z.manifest, outputPart{})
		z.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case parts <- partition{name: name, blocks: blocks, index: index}:
			blocks = nil
			return nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case fl, ok := <-fileLines:
			if !ok {
				return flush()
			}
			line := fl.line
			if len(previous) > 0 && bytes.Compare(previous, line) > 0 {
				return fmt.Errorf("input is not globally sorted: %q follows %q", line, previous)
			}
			previous = append(previous[:0], line...)
			if z.filter != nil && !z.filter(line) {
				continue
			}
			key := z.getKey(line)
			if len(key) == 0 {
				continue
			}
			if len(currentKey) == 0 {
				currentKey = append([]byte(nil), key...)
			}
			if !bytes.Equal(key, currentKey) {
				if err := flush(); err != nil {
					return err
				}
				currentKey = append(currentKey[:0], key...)
			}
			// Producers hand over owned records; writers release them with the part.
			block = append(block, line)
			if len(block) >= z.linesPerBlock {
				blocks = append(blocks, block)
				block = nil
				if len(blocks) >= z.blocksPerPart {
					if err := flush(); err != nil {
						return err
					}
				}
			}
		}
	}
}

// partitionStream stops reading when the writers fall behind. A nil record ends
// the stream; next must return owned storage, which the writer may retain.
func (z *zipnum) partitionStream(ctx context.Context, next func() ([]byte, error)) error {
	ch := make(chan fileLine)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer close(ch)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			line, err := next()
			if err != nil || line == nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- fileLine{line: line}:
			}
		}
	})
	g.Go(func() error { return z.partition(ctx, ch) })
	return g.Wait()
}
