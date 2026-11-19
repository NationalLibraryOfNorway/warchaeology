// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
)

// emitSourcePart reuses a disjoint part when its layout matches the output.
// Legacy names and collisions receive a new UUID and a rewritten part index.
func (z *zipnum) emitSourcePart(ctx context.Context, p sourcePart, matchingLayout bool) error {
	if !matchingLayout {
		return z.partitionParts(ctx, []sourcePart{p}, nil)
	}
	name := p.name
	preserve := z.reserveReusableName(name)
	if !preserve {
		var err error
		name, err = z.allocatePartName()
		if err != nil {
			return err
		}
	}
	if err := linkOrCopy(
		filepath.Join(p.root, partDir, p.name+partExt),
		filepath.Join(z.output, partDir, name+partExt),
	); err != nil {
		return err
	}
	indexSrc := filepath.Join(p.root, indexDir, p.name)
	indexDst := filepath.Join(z.output, indexDir, name)
	if preserve {
		if err := linkOrCopy(indexSrc, indexDst); err != nil {
			return err
		}
	} else if err := rewriteIndexFile(indexSrc, indexDst, name+partExt); err != nil {
		return err
	}
	blocks := make([]blockIndex, len(p.blocks))
	copy(blocks, p.blocks)
	for i := range blocks {
		blocks[i].file = name + partExt
	}
	z.appendManifest(outputPart{
		name:   name,
		blocks: blocks,
		first:  bytes.Clone(p.first), last: bytes.Clone(p.last),
	})
	return ctx.Err()
}

// partitionParts rewrites parts in order, merging any additions up to their last
// record. The caller owns additions, including its lookahead beyond that boundary.
func (z *zipnum) partitionParts(ctx context.Context, parts []sourcePart, additions *rawCursor) (resultErr error) {
	left, err := newPartCursor(parts)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, left.Close()) }()
	var end []byte
	if additions != nil && len(parts) > 0 {
		end = parts[len(parts)-1].last
	}
	return z.partitionMerge(ctx, left, additions, end)
}

// partitionMerge merges two sorted cursors without collecting their records.
// end, when set, limits only the right cursor and leaves its next record unread.
func (z *zipnum) partitionMerge(ctx context.Context, left, right *rawCursor, end []byte) error {
	return z.partitionStream(ctx, func() ([]byte, error) {
		if err := left.Err(); err != nil {
			return nil, err
		}
		var nextRight []byte
		if right != nil {
			if err := right.Err(); err != nil {
				return nil, err
			}
			nextRight = right.Peek()
			if end != nil && bytes.Compare(nextRight, end) > 0 {
				nextRight = nil
			}
		}
		if nextRight != nil && (left.Peek() == nil || bytes.Compare(nextRight, left.Peek()) < 0) {
			return right.Pop(), right.Err()
		}
		return left.Pop(), left.Err()
	})
}

func (z *zipnum) emitMergedParts(ctx context.Context, a, b []sourcePart) (resultErr error) {
	left, err := newPartCursor(a)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, left.Close()) }()
	right, err := newPartCursor(b)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, right.Close()) }()
	return z.partitionMerge(ctx, left, right, nil)
}

// mergeClusters walks two ordered manifests, reusing strictly disjoint parts.
// Overlapping ranges (including equal boundaries) are collected transitively,
// then merged and repartitioned as one group to preserve global ordering.
func (z *zipnum) mergeClusters(ctx context.Context, a, b clusterManifest) error {
	matchA := a.meta.LinesPerBlock == z.linesPerBlock && a.meta.BlocksPerPart == z.blocksPerPart
	matchB := b.meta.LinesPerBlock == z.linesPerBlock && b.meta.BlocksPerPart == z.blocksPerPart
	i, j := 0, 0
	for i < len(a.parts) || j < len(b.parts) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i == len(a.parts) {
			if err := z.emitSourcePart(ctx, b.parts[j], matchB); err != nil {
				return err
			}
			j++
			continue
		}
		if j == len(b.parts) {
			if err := z.emitSourcePart(ctx, a.parts[i], matchA); err != nil {
				return err
			}
			i++
			continue
		}
		pa, pb := a.parts[i], b.parts[j]
		if bytes.Compare(pa.last, pb.first) < 0 {
			if err := z.emitSourcePart(ctx, pa, matchA); err != nil {
				return err
			}
			i++
			continue
		}
		if bytes.Compare(pb.last, pa.first) < 0 {
			if err := z.emitSourcePart(ctx, pb, matchB); err != nil {
				return err
			}
			j++
			continue
		}

		ai, bj := i+1, j+1
		end := pa.last
		if bytes.Compare(pb.last, end) > 0 {
			end = pb.last
		}
		for {
			oldAI, oldBJ := ai, bj
			for ai < len(a.parts) && bytes.Compare(a.parts[ai].first, end) <= 0 {
				if bytes.Compare(a.parts[ai].last, end) > 0 {
					end = a.parts[ai].last
				}
				ai++
			}
			for bj < len(b.parts) && bytes.Compare(b.parts[bj].first, end) <= 0 {
				if bytes.Compare(b.parts[bj].last, end) > 0 {
					end = b.parts[bj].last
				}
				bj++
			}
			if ai == oldAI && bj == oldBJ {
				break
			}
		}
		if err := z.emitMergedParts(ctx, a.parts[i:ai], b.parts[j:bj]); err != nil {
			return err
		}
		i, j = ai, bj
	}
	return nil
}

// addToCluster consumes additions in order, partitioning records in the gaps.
// Parts touched by additions are merged as an overlapping group; others may be reused.
func (z *zipnum) addToCluster(ctx context.Context, base clusterManifest, additions *rawCursor) error {
	matching := base.meta.LinesPerBlock == z.linesPerBlock && base.meta.BlocksPerPart == z.blocksPerPart
	for i := 0; i < len(base.parts); {
		p := base.parts[i]
		if err := ctx.Err(); err != nil {
			return err
		}
		if additions.Peek() != nil && bytes.Compare(additions.Peek(), p.first) < 0 {
			if err := z.partitionStream(ctx, func() ([]byte, error) {
				if additions.Peek() == nil || bytes.Compare(additions.Peek(), p.first) >= 0 {
					return nil, additions.Err()
				}
				return additions.Pop(), additions.Err()
			}); err != nil {
				return err
			}
		}
		if additions.Peek() == nil || bytes.Compare(additions.Peek(), p.last) > 0 {
			if err := z.emitSourcePart(ctx, p, matching); err != nil {
				return err
			}
			i++
			continue
		}
		end := p.last
		j := i + 1
		for j < len(base.parts) && bytes.Compare(base.parts[j].first, end) <= 0 {
			if bytes.Compare(base.parts[j].last, end) > 0 {
				end = base.parts[j].last
			}
			j++
		}
		if err := z.partitionParts(ctx, base.parts[i:j], additions); err != nil {
			return err
		}
		i = j
	}
	return z.partitionStream(ctx, func() ([]byte, error) {
		return additions.Pop(), additions.Err()
	})
}
