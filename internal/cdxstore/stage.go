// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"fmt"
	"strings"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/storagecodec"
)

// CaptureStage buffers a package on disk and indexes its captures by source.
// Callers write and scan one package at a time.
type CaptureStage struct {
	kv       kv
	ops      []mutation
	size     int
	sequence uint64
}

func OpenCaptureStage(ctx context.Context, dir string) (*CaptureStage, error) {
	k, err := openKV(ctx, Options{Format: badgerFormat, Dir: dir, TempDir: dir, Scratch: true})
	if err != nil {
		return nil, err
	}
	return &CaptureStage{kv: k}, nil
}

func (s *CaptureStage) WriteCapture(ctx context.Context, c *cdx.Capture) error {
	encoded, err := storagecodec.Encode(c)
	if err != nil {
		return err
	}
	key := identity(c.GetSource()) + "/" + captureKey(c) + fmt.Sprintf("/%020d", s.sequence)
	s.sequence++
	s.ops = append(s.ops, mutation{key: key, value: encoded})
	s.size += len(key) + len(encoded)
	if len(s.ops) >= 256 || s.size >= 1<<20 {
		return s.Flush(ctx)
	}
	return ctx.Err()
}

func (s *CaptureStage) Flush(ctx context.Context) error {
	if len(s.ops) > 0 {
		if err := s.kv.batch(ctx, s.ops); err != nil {
			return err
		}
		s.ops = nil
		s.size = 0
	}
	return ctx.Err()
}

func (s *CaptureStage) ScanSource(ctx context.Context, source string, emit func(*cdx.Capture) error) error {
	prefix := identity(source) + "/"
	return s.kv.Scan(ctx, prefix+"c/", prefix+"c0", func(key string, value []byte) error {
		capture, err := decodeCapture(strings.TrimPrefix(key, prefix), value)
		if err != nil {
			return err
		}
		return emit(capture)
	})
}

func (s *CaptureStage) Close() error { return s.kv.Close() }
