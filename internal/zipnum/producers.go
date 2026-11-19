// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"bytes"
	"container/heap"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sync/errgroup"
)

type fileLine struct {
	index int
	line  []byte
}

// lineHeap holds one next record per input for the sorted merge.
//
//nolint:recvcheck // container/heap uses pointer Push/Pop and slice accessors.
type lineHeap []fileLine

func (h lineHeap) Len() int { return len(h) }

func (h lineHeap) Less(i, j int) bool { return bytes.Compare(h[i].line, h[j].line) < 0 }

func (h lineHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *lineHeap) Push(x any) { *h = append(*h, x.(fileLine)) }

func (h *lineHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// mergeSort merges sorted inputs through a heap, preserving duplicate records.
// Each producer checks its own ordering; cancellation and errors stop the group.
func mergeSort(ctx context.Context, inputFiles []string, out chan<- fileLine) error {
	defer close(out)
	g, groupCtx := errgroup.WithContext(ctx)
	channels := make([]chan fileLine, len(inputFiles))
	for i, inputPath := range inputFiles {
		info, err := os.Stat(inputPath)
		if err != nil {
			return err
		}
		path := inputPath
		if info.IsDir() {
			path = filepath.Join(path, indexFile)
		}
		channels[i] = make(chan fileLine, 128)
		ch := channels[i]
		g.Go(func() error {
			defer close(ch)
			var previous []byte
			return readInputLines(path, func(line []byte) error {
				if len(previous) > 0 && bytes.Compare(previous, line) > 0 {
					return fmt.Errorf("input %q is not sorted: %q follows %q", path, line, previous)
				}
				previous = append(previous[:0], line...)
				select {
				case <-groupCtx.Done():
					return groupCtx.Err()
				case ch <- fileLine{index: i, line: line}:
					return nil
				}
			})
		})
	}

	h := make(lineHeap, 0, len(channels))
	heap.Init(&h)
	for _, ch := range channels {
		select {
		case <-groupCtx.Done():
			return g.Wait()
		case fl, ok := <-ch:
			if ok {
				heap.Push(&h, fl)
			}
		}
	}
	for h.Len() > 0 {
		smallest := heap.Pop(&h).(fileLine)
		select {
		case <-groupCtx.Done():
			return g.Wait()
		case out <- smallest:
		}
		select {
		case <-groupCtx.Done():
			return g.Wait()
		case next, ok := <-channels[smallest.index]:
			if ok {
				heap.Push(&h, next)
			}
		}
	}
	return g.Wait()
}

// seq streams inputs in the given order and checks ordering across file boundaries.
func seq(ctx context.Context, inputFiles []string, out chan<- fileLine) error {
	defer close(out)
	var previous []byte
	for _, path := range inputFiles {
		err := readInputLines(path, func(line []byte) error {
			if len(previous) > 0 && bytes.Compare(previous, line) > 0 {
				return fmt.Errorf("input is not globally sorted: %q follows %q", line, previous)
			}
			previous = append(previous[:0], line...)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case out <- fileLine{line: line}:
				return nil
			}
		})
		if err != nil {
			return err
		}
	}
	return nil
}
