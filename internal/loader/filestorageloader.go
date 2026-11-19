// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/nationallibraryofnorway/gowarc/v3"
	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
)

type FileStorageLoader struct {
	ValidateSource func(context.Context, string, string, fs.FileInfo) error
}

type captureContextKey struct{}

func WithCapture(ctx context.Context, c *index.Capture) context.Context {
	return context.WithValue(ctx, captureContextKey{}, c)
}

func expectedGeneration(ctx context.Context, ref string) string {
	c, _ := ctx.Value(captureContextKey{}).(*index.Capture)
	if c.GetRef() != ref {
		return ""
	}
	return c.GetGeneration()
}

func (f FileStorageLoader) RecordByStorageRef(ctx context.Context, storageRef string) (record gowarc.WarcRecord, err error) {
	filename, offset, err := index.ParseStorageRef(storageRef)
	if err != nil {
		return nil, err
	}
	file, err := filewalker.OpenSource(ctx, filename)
	if err != nil {
		return nil, err
	}
	if f.ValidateSource != nil {
		if err := f.checkSource(ctx, filename, expectedGeneration(ctx, storageRef), file); err != nil {
			return nil, errors.Join(err, file.Close())
		}
	}
	wf, err := gowarc.NewWarcFileReaderFromStream(file, offset, gowarc.WithSyntaxErrorPolicy(gowarc.ErrIgnore), gowarc.WithSpecViolationPolicy(gowarc.ErrWarn))
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.Join(err, wf.Close())
	}
	rec, err := wf.Next()
	if err == nil && f.ValidateSource != nil {
		err = f.checkSource(ctx, filename, expectedGeneration(ctx, storageRef), file)
		if err == nil && rec.Offset != offset {
			err = fmt.Errorf("no WARC record at indexed offset %d", offset)
		}
	}
	closeErr := wf.Close()
	if err != nil || closeErr != nil {
		_ = rec.Close()
		return nil, errors.Join(err, closeErr)
	}
	return rec.WarcRecord, nil
}

func (f FileStorageLoader) checkSource(ctx context.Context, source, generation string, file fs.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	return f.ValidateSource(ctx, source, generation, info)
}
