// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filter"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warc"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/workerpool"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	CalculateHash     = "calculate-hash"
	CalculateHashHelp = `calculate hash of output file. The hash is made available to the close output file hook as WARC_HASH. Valid values: md5, sha1, sha256, sha512`
)

type ValidateOptions struct {
	paths              []string
	outputDir          string
	concurrency        int
	recordNum          int
	recordCount        int
	offset             int64
	force              bool
	hashFunction       string
	FileIndex          *index.FileIndex
	filter             *filter.RecordFilter
	FileWalker         *filewalker.FileWalker
	continueOnError    bool
	warcRecordOptions  []gowarc.WarcRecordOption
	openInputFileHook  hooks.OpenInputFileHook
	closeInputFileHook hooks.CloseInputFileHook
}

func NewValidateFlags() ValidateFlags {
	return ValidateFlags{
		OutputHookFlags: &flag.OutputHookFlags{},
		InputHookFlags:  &flag.InputHookFlags{},
	}
}

// Close releases the resources owned by the options. Call it after Run returns.
// Repeated calls are safe; Close must not run concurrently with Run or itself.
func (o *ValidateOptions) Close() error {
	var err error
	if o.FileIndex != nil {
		err = errors.Join(err, o.FileIndex.Close())
		o.FileIndex = nil
	}
	if o.FileWalker != nil {
		err = errors.Join(err, o.FileWalker.Close())
		o.FileWalker = nil
	}
	return err
}

type ValidateFlags struct {
	IndexFlags            flag.IndexFlags
	OutputHookFlags       *flag.OutputHookFlags
	InputHookFlags        *flag.InputHookFlags
	FileWalkerFlags       flag.FileWalkerFlags
	FilterFlags           flag.FilterFlags
	WarcRecordOptionFlags flag.WarcRecordOptionFlags
	ConcurrencyFlags      flag.ConcurrencyFlags
	ErrorFlags            flag.ErrorFlags
	WarcIteratorFlags     flag.WarcIteratorFlags
}

func (f ValidateFlags) AddFlags(cmd *cobra.Command) {
	f.FileWalkerFlags.AddFlags(cmd)
	f.FilterFlags.AddFlags(cmd)
	f.IndexFlags.AddFlags(cmd)
	f.WarcRecordOptionFlags.AddFlags(cmd)
	f.InputHookFlags.AddFlags(cmd)
	f.ConcurrencyFlags.AddFlags(cmd)
	f.ErrorFlags.AddFlags(cmd)
	f.WarcIteratorFlags.AddFlags(cmd)

	cmd.Flags().String(CalculateHash, "", CalculateHashHelp)
}

func (f ValidateFlags) OutputDir() string {
	return viper.GetString(flag.OutputDir)
}

func (f ValidateFlags) HashFunction() string {
	return viper.GetString(CalculateHash)
}

func (f ValidateFlags) ToOptions() (_ *ValidateOptions, err error) {
	filter, err := f.FilterFlags.ToFilter()
	if err != nil {
		return nil, fmt.Errorf("failed to create filter: %w", err)
	}
	fileList, err := flag.ReadSrcFileList(f.FileWalkerFlags.SrcFileListFlags.SrcFileList())
	if err != nil {
		return nil, fmt.Errorf("failed to read from source file: %w", err)
	}
	openInputFileHook, err := f.InputHookFlags.ToOpenInputFileHook()
	if err != nil {
		return nil, fmt.Errorf("failed to create open input file hook: %w", err)
	}
	closeInputFileHook, err := f.InputHookFlags.ToCloseInputFileHook()
	if err != nil {
		return nil, fmt.Errorf("failed to create close input file hook: %w", err)
	}

	o := &ValidateOptions{
		paths:              fileList,
		continueOnError:    f.ErrorFlags.ContinueOnError(),
		filter:             filter,
		hashFunction:       f.HashFunction(),
		concurrency:        f.ConcurrencyFlags.Concurrency(),
		outputDir:          f.OutputDir(),
		recordNum:          f.WarcIteratorFlags.RecordNum(),
		recordCount:        f.WarcIteratorFlags.Limit(),
		force:              f.WarcIteratorFlags.Force(),
		offset:             f.WarcIteratorFlags.Offset(),
		warcRecordOptions:  f.WarcRecordOptionFlags.ToWarcRecordOptions(),
		openInputFileHook:  openInputFileHook,
		closeInputFileHook: closeInputFileHook,
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, o.Close())
		}
	}()

	if f.IndexFlags.KeepIndex() {
		o.FileIndex, err = f.IndexFlags.ToFileIndex()
		if err != nil {
			return nil, fmt.Errorf("failed to create file index: %w", err)
		}
		profile, err := json.Marshal([]any{o.force, o.recordNum, o.recordCount, o.offset, o.hashFunction,
			f.WarcRecordOptionFlags.StrictValidation(), f.WarcRecordOptionFlags.LenientValidation(), f.WarcRecordOptionFlags.LaxHostParsing(),
			f.FilterFlags.ResponseCode(), f.FilterFlags.RecordId(), f.FilterFlags.RecordType(), f.FilterFlags.MimeType()})
		if err != nil {
			return nil, err
		}
		o.FileIndex.SetValidationProfile(string(profile))
	}

	o.FileWalker, err = f.FileWalkerFlags.ToFileWalker()
	if err != nil {
		return nil, fmt.Errorf("failed to create file walker: %w", err)
	}
	return o, nil
}

func NewCmdValidate() *cobra.Command {
	flags := NewValidateFlags()

	var cmd = &cobra.Command{
		Use:   "validate FILE/DIR ...",
		Short: "Validate WARC files",
		Long:  ``,
		RunE: func(cmd *cobra.Command, args []string) error {
			o, err := flags.ToOptions()
			if err != nil {
				return err
			}
			defer func() {
				if err := o.Close(); err != nil {
					slog.Error("Close options", "error", err)
				}
			}()
			err = o.Complete(cmd, args)
			if err != nil {
				return err
			}
			err = o.Validate()
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			return o.Run()
		},
		ValidArgsFunction: flag.SuffixCompletionFn,
	}

	flags.AddFlags(cmd)

	return cmd
}

func (o *ValidateOptions) Complete(cmd *cobra.Command, args []string) error {
	o.paths = append(o.paths, args...)
	return nil
}

func (o *ValidateOptions) Validate() error {
	if len(o.paths) == 0 {
		return errors.New("missing file or directory name")
	}
	return nil
}

// Run processes the inputs and waits for workers to finish.
// The caller must close the options after Run returns.
func (o *ValidateOptions) Run() (runErr error) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	done := make(chan struct{})
	var processingErr error

	defer func() {
		<-done
		runErr = errors.Join(runErr, processingErr, ctx.Err())
	}()

	results := make(chan stat.Result)
	go func() {
		defer close(done)

		stats := stat.NewStats()
		defer func() {
			slog.Info("Total", "files", stats.Files, "errors", stats.Errors, "records", stats.Records)
		}()

		for result := range results {
			slog := slog.With("path", result.Name())
			for _, err := range result.Errors() {
				if recordErr, ok := errors.AsType[warc.RecordError](err); ok {
					slog.Error("Validation error", "error", recordErr.Error(), "offset", recordErr.Offset())
				} else {
					slog.Error("Validation error", "error", err.Error())
				}
			}
			slog.Info("Validated file", "errors", result.ErrorCount(), "records", result.Records())
			stats.Merge(result)
		}
		if stats.Errors > 0 {
			processingErr = errors.New("processing completed with errors")
		}
	}()
	defer close(results)

	workerPool := workerpool.New(ctx, o.concurrency)
	o.FileWalker.WaitForReaders = workerPool.Wait
	defer workerPool.CloseWait()

	return o.FileWalker.WalkInputs(ctx, o.paths, o.continueOnError, func(fs afero.Fs, path string) error {
		return workerPool.Submit(func() {
			result, err := filewalker.Preposterous(fs, path, o.openInputFileHook, o.closeInputFileHook, o.FileIndex, func(fs afero.Fs, path string) (stat.Result, error) { return o.handleFile(ctx, fs, path) })
			if errors.Is(err, filewalker.ErrSkipFile) {
				return
			} else if err != nil {
				if !o.continueOnError {
					cancel()
				}
				if result == nil {
					result = stat.NewResult(path)
				}
				result.AddError(err)
			}

			results <- result
		})
	})
}

func (o *ValidateOptions) handleFile(ctx context.Context, fs afero.Fs, path string) (stat.Result, error) {
	result := stat.NewResult(path)

	file, err := fs.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	countingReader := NewCountingReader(file, o.hashFunction)
	defer func() {
		result.SetHash(countingReader.Hash())
	}()

	warcFileReader, err := gowarc.NewWarcFileReaderFromStream(countingReader, o.offset, o.warcRecordOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to create warc file reader: %w", err)
	}
	defer func() { _ = warcFileReader.Close() }()

	raw := warc.Records(ctx, warcFileReader, o.force, path)
	if !o.force && o.offset == 0 && o.recordNum <= 0 && o.recordCount <= 0 {
		info, err := file.Stat()
		if err != nil {
			return result, err
		}
		raw = warc.CheckCoverage(raw, file, info.Size())
	}
	records := warc.Compose(raw, o.filter, o.recordNum, o.recordCount)
	for record, err := range records {
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}

		result.IncrRecords()
		for _, err := range record.Validation {
			result.AddError(warc.ErrorFrom(record, err))
		}
		if err := record.Close(); err != nil {
			return result, err
		}
	}
	return result, nil
}
