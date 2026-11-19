// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package nedlib

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/diskspace"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/version"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warc"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcwriterconfig"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/workerpool"
	"github.com/nationallibraryofnorway/warchaeology/v5/nedlibreader"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type ConvertNedlibOptions struct {
	Paths              []string
	FileWalker         *filewalker.FileWalker
	WarcWriterConfig   *warcwriterconfig.WarcWriterConfig
	FileIndex          *index.FileIndex
	MinWARCDiskFree    int64
	Concurrency        int
	ContinueOnError    bool
	WarcRecordOptions  []gowarc.WarcRecordOption
	OpenInputFileHook  hooks.OpenInputFileHook
	CloseInputFileHook hooks.CloseInputFileHook
}

// Close releases the resources owned by the options. Call it after Run returns.
// Repeated calls are safe; Close must not run concurrently with Run or itself.
func (o *ConvertNedlibOptions) Close() error {
	var err error
	if o.WarcWriterConfig != nil {
		err = errors.Join(err, o.WarcWriterConfig.Close())
		o.WarcWriterConfig = nil
	}
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

type ConvertNedlibFlags struct {
	FileWalkerFlags       flag.FileWalkerFlags
	WarcWriterConfigFlags *flag.WarcWriterConfigFlags
	IndexFlags            flag.IndexFlags
	OutputHookFlags       *flag.OutputHookFlags
	InputHookFlags        *flag.InputHookFlags
	ConcurrencyFlags      flag.ConcurrencyFlags
	ErrorFlags            flag.ErrorFlags
}

func NewConvertNedlibFlags() ConvertNedlibFlags {
	return ConvertNedlibFlags{
		OutputHookFlags:       &flag.OutputHookFlags{},
		InputHookFlags:        &flag.InputHookFlags{},
		WarcWriterConfigFlags: &flag.WarcWriterConfigFlags{},
	}
}

func (f ConvertNedlibFlags) AddFlags(cmd *cobra.Command) {
	f.FileWalkerFlags.AddFlags(cmd, flag.WithDefaultSuffixes([]string{".meta"}))
	f.WarcWriterConfigFlags.AddFlags(cmd, flag.WithDefaultFilePrefix("nedlib_"))
	f.IndexFlags.AddFlags(cmd)
	f.OutputHookFlags.AddFlags(cmd)
	f.InputHookFlags.AddFlags(cmd)
	f.ConcurrencyFlags.AddFlags(cmd)
	f.ErrorFlags.AddFlags(cmd)
}

func (f ConvertNedlibFlags) ToOptions() (_ *ConvertNedlibOptions, err error) {
	// The Nedlib data structure does not support direct filename transformations.
	// Instead, we employ a custom generator that treats the input filename as a date.
	// When we request a new warcwriter, we submit a synthetic fromFilename based on the date of the first record.
	viper.Set(flag.NameGenerator, "nedlib")

	fileList, err := flag.ReadSrcFileList(f.FileWalkerFlags.SrcFileListFlags.SrcFileList())
	if err != nil {
		return nil, fmt.Errorf("failed to read from source file: %w", err)
	}

	o := &ConvertNedlibOptions{
		Paths:           fileList,
		ContinueOnError: f.ErrorFlags.ContinueOnError(),
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, o.Close())
		}
	}()

	o.WarcWriterConfig, err = f.WarcWriterConfigFlags.ToWarcWriterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create warc writer config: %w", err)
	}
	warcWriterConfig := o.WarcWriterConfig

	warcInfoFunc := func(wr gowarc.WarcRecordBuilder) error {
		warcHeader := &gowarc.WarcFields{}
		warcHeader.Set("software", version.SoftwareVersion())
		warcHeader.Set("description", "Converted from Nedlib")
		if hostname, err := os.Hostname(); err != nil {
			return err
		} else {
			warcHeader.Set("host", hostname)
		}
		_, err := wr.WriteString(warcHeader.String())
		return err
	}
	warcWriterConfig.WarcFileWriterOptions = append(warcWriterConfig.WarcFileWriterOptions, gowarc.WithWarcInfoFunc(warcInfoFunc))

	o.WarcRecordOptions = []gowarc.WarcRecordOption{
		gowarc.WithVersion(warcWriterConfig.WarcVersion),
		gowarc.WithAddMissingDigest(true),
		gowarc.WithFixDigest(true),
		gowarc.WithFixContentLength(true),
		gowarc.WithAddMissingContentLength(true),
	}

	if f.IndexFlags.KeepIndex() {
		o.FileIndex, err = f.IndexFlags.ToFileIndex()
		if err != nil {
			return nil, fmt.Errorf("failed to create file index: %w", err)
		}
	}

	o.FileWalker, err = f.FileWalkerFlags.ToFileWalker()
	if err != nil {
		return nil, fmt.Errorf("failed to create file walker: %w", err)
	}
	return o, nil
}

func NewCmdConvertNedlib() *cobra.Command {
	flags := NewConvertNedlibFlags()

	cmd := &cobra.Command{
		Use:   "nedlib FILE/DIR ...",
		Short: "Convert directory with files harvested with Nedlib into warc files",
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
	}

	flags.AddFlags(cmd)

	return cmd
}

func (o *ConvertNedlibOptions) Complete(cmd *cobra.Command, args []string) error {
	o.Paths = append(o.Paths, args...)
	return nil
}

func (o *ConvertNedlibOptions) Validate() error {
	if len(o.Paths) == 0 {
		return fmt.Errorf("missing file or directory name")
	}
	return nil
}

// Run processes the inputs and waits for workers to finish.
// The caller must close the options after Run returns.
func (o *ConvertNedlibOptions) Run() (runErr error) {
	done := make(chan struct{})
	var processingErr error

	defer func() {
		<-done
		runErr = errors.Join(runErr, processingErr)
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

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
			slog.Info("Converted file", "errors", result.ErrorCount(), "records", result.Records())
			stats.Merge(result)
		}
		if stats.Errors > 0 {
			processingErr = errors.New("processing completed with errors")
		}
	}()
	defer close(results)

	workerPool := workerpool.New(ctx, o.Concurrency)
	defer workerPool.CloseWait()

	for _, path := range o.Paths {
		err := o.FileWalker.Walk(ctx, path, func(fs afero.Fs, path string, err error) error {
			if err != nil {
				return err
			}

			return workerPool.Submit(func() {
				// Assert WARC disk has enough free space
				if o.MinWARCDiskFree > 0 {
					diskFree, err := diskspace.Free(o.WarcWriterConfig.OutDir)
					if err != nil {
						cancel()
						slog.Error("Failed to get free space on device", "path", o.WarcWriterConfig.OutDir, "error", err)
						return
					}
					if diskFree < o.MinWARCDiskFree {
						cancel()
						slog.Error("Not enough free space on device", "bytesFree", diskFree, "path", o.WarcWriterConfig.OutDir)
						return
					}
				}

				result, err := filewalker.Preposterous(fs, path, o.OpenInputFileHook, o.CloseInputFileHook, o.FileIndex, o.handleFile)
				if errors.Is(err, filewalker.ErrSkipFile) {
					return
				} else if err != nil {
					if !o.ContinueOnError {
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
		if err != nil {
			return err
		}
	}
	return nil
}

func (o *ConvertNedlibOptions) handleFile(fs afero.Fs, fileName string) (stat.Result, error) {
	result := stat.NewResult(fileName)

	nedlibReader, err := nedlibreader.NewNedlibReader(fs, fileName, o.WarcWriterConfig.DefaultTime, o.WarcRecordOptions...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = nedlibReader.Close() }()

	var writer *gowarc.WarcFileWriter
	if o.WarcWriterConfig.OneToOneWriter {
		defer func() {
			if writer != nil {
				_ = writer.Close()
			}
		}()
	}

	records := warc.Compose(nedlibReader.Records(), nil, 0, 0)
	for record, err := range records {
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}

		warcDate, err := record.WarcRecord.WarcHeader().GetTime(gowarc.WarcDate)
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}

		syntheticFileName := timestamp.Format14(warcDate)

		writer, err := o.WarcWriterConfig.GetWarcWriter(syntheticFileName, warcDate)
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}

		err = o.handleRecord(writer, record, result)
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}
	}

	return result, nil
}

func (o *ConvertNedlibOptions) handleRecord(w *gowarc.WarcFileWriter, record gowarc.Record, result stat.Result) error {
	defer record.Close()

	result.IncrRecords()

	if len(record.Validation) > 0 {
		for _, err := range record.Validation {
			result.AddError(warc.ErrorFrom(record, err))
		}
	}

	writeResponse := w.Write(record.WarcRecord)
	if len(writeResponse) > 0 {
		return writeResponse[0].Err
	}
	return nil
}
