// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package conversion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/nationallibraryofnorway/gowarc/v4"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/diskspace"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcwriterconfig"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/workerpool"
	"github.com/spf13/afero"
)

type RunOptions struct {
	Walker      *filewalker.FileWalker
	Index       *index.FileIndex
	Writer      *warcwriterconfig.WarcWriterConfig
	Open        hooks.OpenInputFileHook
	Close       hooks.CloseInputFileHook
	Concurrency int
	MinimumFree int64
	Continue    bool
	Repair      bool
}

func (o RunOptions) Run(paths []string, handle FileHandler) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	pool := workerpool.New(ctx, o.Concurrency)
	o.Walker.WaitForReaders = pool.Wait
	pending := make(map[string]stat.Result)
	var mu sync.Mutex
	var processingErr, walkErr error
	stats := stat.NewStats()
	walkErr = o.Walker.WalkInputs(ctx, paths, o.Continue, func(fs afero.Fs, path string) error {
		return pool.Submit(func() {
			key, result, err := o.process(ctx, fs, path, handle)
			if errors.Is(err, filewalker.ErrSkipFile) {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if result == nil {
					result = stat.NewResult(path)
				}
				result.AddError(err)
			}
			for _, issue := range result.Errors() {
				slog.Error("Conversion error", "path", path, "error", issue)
				if processingErr == nil {
					processingErr = fmt.Errorf("%s: %w", path, issue)
				}
			}
			if err != nil && (!o.Continue || errors.Is(err, errDisk)) {
				cancel()
			}
			stats.Merge(result)
			slog.Info("Converted file", "path", path, "records", result.Records(), "errors", result.ErrorCount())
			if key != "" {
				pending[key] = result
			}
		})
	})
	pool.CloseWait()
	slog.Info("Total", "files", stats.Files, "records", stats.Records, "errors", stats.Errors)
	if stats.Errors > 0 && processingErr == nil {
		processingErr = fmt.Errorf("conversion completed with %d errors", stats.Errors)
	}
	if err := errors.Join(walkErr, processingErr, ctx.Err(), o.Writer.Publish()); err != nil {
		return err
	}
	for key, result := range pending {
		if err := o.Index.SaveFileStats(key, result); err != nil {
			return err
		}
	}
	if len(pending) > 0 {
		return o.Index.Sync()
	}
	return nil
}

var errDisk = errors.New("output disk space check failed")

func (o RunOptions) process(ctx context.Context, fs afero.Fs, path string, handle FileHandler) (string, stat.Result, error) {
	var key string
	if o.Index != nil {
		var err error
		key, err = filewalker.CheckpointKey(fs, path)
		if err != nil {
			return "", nil, err
		}
		cached, err := o.Index.GetFileStats(key)
		if cached != nil || err != nil {
			return "", cached, err
		}
	}
	if o.MinimumFree > 0 {
		free, err := diskspace.Free(o.Writer.OutDir)
		if err != nil {
			return "", nil, fmt.Errorf("%w at %s: %w", errDisk, o.Writer.OutDir, err)
		}
		if free < o.MinimumFree {
			return "", nil, fmt.Errorf("%w: %s has %d bytes free, requires %d", errDisk, o.Writer.OutDir, free, o.MinimumFree)
		}
	}
	result, err := o.ConvertFile(ctx, fs, path, handle)
	if key != "" && o.Writer.OneToOneWriter {
		if err == nil && result != nil && (result.ErrorCount() == 0 || o.Repair) {
			err = o.Index.SaveFileStats(key, result)
			if err == nil {
				err = o.Index.Sync()
			}
		}
		return "", result, err
	}
	return key, result, err
}

type GetWriter func(string, time.Time) (*gowarc.WarcFileWriter, error)
type FileHandler func(context.Context, afero.Fs, string, GetWriter) (stat.Result, error)

// ConvertFile scopes publication to one input, including its input hooks.
func (o RunOptions) ConvertFile(ctx context.Context, fs afero.Fs, path string, handle FileHandler) (stat.Result, error) {
	var finish func(bool) error
	get := func(name string, date time.Time) (*gowarc.WarcFileWriter, error) {
		writer, done, err := o.Writer.WriterForInput(ctx, name, date)
		finish = done
		return writer, err
	}
	result, err := filewalker.PrePostHookContext(ctx, fs, path, o.Open, o.Close, func(fs afero.Fs, path string) (stat.Result, error) {
		return handle(ctx, fs, path, get)
	})
	if finish != nil {
		err = errors.Join(err, ctx.Err())
		err = errors.Join(err, finish(err == nil && result != nil && (result.ErrorCount() == 0 || o.Repair)))
	}
	return result, err
}
