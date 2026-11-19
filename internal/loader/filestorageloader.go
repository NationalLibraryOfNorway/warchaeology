// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"context"
	"errors"

	"github.com/nationallibraryofnorway/gowarc/v3"
	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
)

type FilePathResolver interface {
	PathByFilename(filename string) (path string, err error)
}

type FileStorageLoader struct {
	FilePathResolver
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
	wf, err := gowarc.NewWarcFileReaderFromStream(file, offset, gowarc.WithSyntaxErrorPolicy(gowarc.ErrIgnore), gowarc.WithSpecViolationPolicy(gowarc.ErrWarn))
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.Join(err, wf.Close())
	}
	rec, err := wf.Next()
	closeErr := wf.Close()
	if err != nil || closeErr != nil {
		_ = rec.Close()
		return nil, errors.Join(err, closeErr)
	}
	return rec.WarcRecord, nil
}
