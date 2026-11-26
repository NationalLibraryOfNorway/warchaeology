// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warc

import (
	"context"
	"errors"
	"fmt"

	"github.com/nationallibraryofnorway/gowarc/v4"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/convert/internal/conversion"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warc"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcwriterconfig"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
)

type ConvertWarcOptions struct {
	Paths              []string
	Concurrency        int
	MinWARCDiskFree    int64
	Repair             bool
	ContinueOnError    bool
	Offset             int64
	RecordNum          int
	RecordCount        int
	Force              bool
	WarcRecordOptions  []gowarc.WarcRecordOption
	WarcWriterConfig   *warcwriterconfig.WarcWriterConfig
	FileWalker         *filewalker.FileWalker
	FileIndex          *index.FileIndex
	OpenInputFileHook  hooks.OpenInputFileHook
	CloseInputFileHook hooks.CloseInputFileHook
}

// Close releases the resources owned by the options. Call it after Run returns.
// Repeated calls are safe; Close must not run concurrently with Run or itself.
func (o *ConvertWarcOptions) Close() error {
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

type ConvertWarcFlags struct {
	FileWalkerFlags       flag.FileWalkerFlags
	IndexFlags            flag.IndexFlags
	WarcRecordOptionFlags flag.WarcRecordOptionFlags
	WarcWriterConfigFlags *flag.WarcWriterConfigFlags
	OutputHookFlags       *flag.OutputHookFlags
	InputHookFlags        *flag.InputHookFlags
	WarcIteratorFlags     flag.WarcIteratorFlags
	UtilFlags             flag.UtilFlags
	RepairFlags           flag.RepairFlags
	ConcurrencyFlags      flag.ConcurrencyFlags
	ErrorFlags            flag.ErrorFlags
}

func NewConvertWarcFlags() ConvertWarcFlags {
	return ConvertWarcFlags{
		OutputHookFlags:       &flag.OutputHookFlags{},
		InputHookFlags:        &flag.InputHookFlags{},
		WarcWriterConfigFlags: &flag.WarcWriterConfigFlags{},
	}
}

func (f ConvertWarcFlags) AddFlags(cmd *cobra.Command) {
	f.FileWalkerFlags.AddFlags(cmd, flag.WithDefaultSuffixes([]string{".warc", ".warc.gz"}))
	f.IndexFlags.AddFlags(cmd)
	f.WarcRecordOptionFlags.AddFlags(cmd)
	f.WarcWriterConfigFlags.AddFlags(cmd)
	f.OutputHookFlags.AddFlags(cmd)
	f.InputHookFlags.AddFlags(cmd)
	f.UtilFlags.AddFlags(cmd)
	if !f.RepairFlags.Always {
		f.RepairFlags.AddFlags(cmd)
	}
	f.ConcurrencyFlags.AddFlags(cmd)
	f.ErrorFlags.AddFlags(cmd)
	f.WarcIteratorFlags.AddFlags(cmd)
}

func (f ConvertWarcFlags) ToConvertWarcOptions() (_ *ConvertWarcOptions, err error) {
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

	o := &ConvertWarcOptions{
		Repair:             f.RepairFlags.Repair(),
		Concurrency:        f.ConcurrencyFlags.Concurrency(),
		MinWARCDiskFree:    f.UtilFlags.MinFreeDisk(),
		Paths:              fileList,
		RecordNum:          f.WarcIteratorFlags.RecordNum(),
		RecordCount:        f.WarcIteratorFlags.Limit(),
		Force:              f.WarcIteratorFlags.Force(),
		Offset:             f.WarcIteratorFlags.Offset(),
		OpenInputFileHook:  openInputFileHook,
		CloseInputFileHook: closeInputFileHook,
		ContinueOnError:    f.ErrorFlags.ContinueOnError(),
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

	o.WarcRecordOptions = []gowarc.WarcRecordOption{
		gowarc.WithVersion(o.WarcWriterConfig.WarcVersion),
	}
	o.WarcRecordOptions = append(o.WarcRecordOptions, f.RepairFlags.ToWarcRecordOptions()...)
	o.WarcRecordOptions = append(o.WarcRecordOptions, f.WarcRecordOptionFlags.ToWarcRecordOptions()...)

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

func NewCmdConvertWarc() *cobra.Command {
	return newConversion(false)
}

func NewCmdRepair() *cobra.Command { return newConversion(true) }

func newConversion(repair bool) *cobra.Command {
	flags := NewConvertWarcFlags()
	flags.RepairFlags.Always = repair

	var cmd = &cobra.Command{
		Use:   "warc FILE/DIR ...",
		Short: "Rewrite WARCs with different output settings",
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			if repair && !flag.Explicit(cmd, flag.OutputDir) {
				return errors.New("repair requires --output-dir")
			}
			if !repair && flags.RepairFlags.Repair() {
				fmt.Fprintln(cmd.ErrOrStderr(), "deprecated: convert warc --repair (-R); use warc repair")
			}
			o, err := flags.ToConvertWarcOptions()
			if err != nil {
				return err
			}
			defer func() {
				runErr = errors.Join(runErr, o.Close())
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

	cmd.Long = "Rewrite WARC files with the selected output settings, including compression."
	if repair {
		cmd.Use = "repair FILE/DIR ..."
		cmd.Short = "Correct records using their own bytes"
		cmd.Long = "Write corrected records to the output directory. Input defects can still be reported even when the corrected record is written."
	}
	flags.AddFlags(cmd)

	return cmd
}

func (o *ConvertWarcOptions) Complete(cmd *cobra.Command, args []string) error {
	o.Paths = append(o.Paths, args...)
	return nil
}

func (o *ConvertWarcOptions) Validate() error {
	if len(o.Paths) == 0 {
		return errors.New("missing file or directory name")
	}
	return nil
}

// Run processes the inputs and waits for workers to finish.
// The caller must close the options after Run returns.
func (o *ConvertWarcOptions) Run() error {
	runner := conversion.RunOptions{
		Repair:      o.Repair,
		Walker:      o.FileWalker,
		Index:       o.FileIndex,
		Writer:      o.WarcWriterConfig,
		Open:        o.OpenInputFileHook,
		Close:       o.CloseInputFileHook,
		Concurrency: o.Concurrency,
		MinimumFree: o.MinWARCDiskFree,
		Continue:    o.ContinueOnError,
	}
	return runner.Run(o.Paths, o.handleFile)
}

func (o *ConvertWarcOptions) handleFile(ctx context.Context, fs afero.Fs, path string, getWriter conversion.GetWriter) (result stat.Result, resultErr error) {
	file, err := fs.Open(path)
	if err != nil {
		return nil, err
	}

	warcFileReader, err := gowarc.NewWarcFileReaderFromStream(file, o.Offset, o.WarcRecordOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to create warc file reader: %w", errors.Join(err, file.Close()))
	}
	defer func() { resultErr = errors.Join(resultErr, warcFileReader.Close()) }()

	var writer *gowarc.WarcFileWriter

	result = stat.NewResult(path)

	raw := warc.Records(ctx, warcFileReader, o.Force, path)
	if !o.Force && o.Offset == 0 && o.RecordNum <= 0 && o.RecordCount <= 0 {
		info, err := file.Stat()
		if err != nil {
			return result, err
		}
		raw = warc.CheckCoverage(raw, file, info.Size())
	}
	records := warc.Compose(raw, nil, o.RecordNum, o.RecordCount)
	for record, err := range records {
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}

		if writer == nil || !o.WarcWriterConfig.OneToOneWriter {
			warcDate, err := record.WarcRecord.WarcHeader().GetTime(gowarc.WarcDate)
			if err != nil {
				return result, errors.Join(warc.ErrorFrom(record, err), record.Close())
			}
			writer, err = getWriter(path, warcDate)
			if err != nil {
				return result, errors.Join(warc.ErrorFrom(record, err), record.Close())
			}
		}
		err = o.handleRecord(writer, record, result)
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}
	}
	return result, nil
}

func (o *ConvertWarcOptions) handleRecord(warcFileWriter *gowarc.WarcFileWriter, record gowarc.Record, result stat.Result) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, record.Close()) }()

	result.IncrRecords()

	if len(record.Validation) > 0 {
		for _, err := range record.Validation {
			result.AddError(warc.ErrorFrom(record, err))
		}
	}

	warcRecord := record.WarcRecord
	warcRecordBuilder := gowarc.NewRecordBuilder(warcRecord.Type(), o.WarcRecordOptions...)

	for _, warcField := range *warcRecord.WarcHeader() {
		if warcField.Name != gowarc.WarcType {
			warcRecordBuilder.AddWarcHeader(warcField.Name, warcField.Value)
		}
	}
	ioReader, err := warcRecord.Block().RawBytes()
	if err != nil {
		return err
	}
	_, err = warcRecordBuilder.ReadFrom(ioReader)
	if err != nil {
		return err
	}
	warcRecord, _, err = warcRecordBuilder.Build()
	if err != nil {
		return err
	}
	defer func() {
		_ = warcRecord.Close()
	}()
	if writeResponse := warcFileWriter.Write(warcRecord); len(writeResponse) > 0 {
		return writeResponse[0].Err
	}
	return nil
}
