// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package arc

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/arcreader"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/convert/internal/conversion"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/version"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warc"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcwriterconfig"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type ConvertArcOptions struct {
	Paths              []string
	Concurrency        int
	MinWARCDiskFree    int64
	WarcWriterConfig   *warcwriterconfig.WarcWriterConfig
	WarcRecordOptions  []gowarc.WarcRecordOption
	FileWalker         *filewalker.FileWalker
	ContinueOnError    bool
	FileIndex          *index.FileIndex
	OpenInputFileHook  hooks.OpenInputFileHook
	CloseInputFileHook hooks.CloseInputFileHook
}

// Close releases the resources owned by the options. Call it after Run returns.
// Repeated calls are safe; Close must not run concurrently with Run or itself.
func (o *ConvertArcOptions) Close() error {
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

type ConvertArcFlags struct {
	FileWalkerFlags       flag.FileWalkerFlags
	IndexFlags            flag.IndexFlags
	WarcWriterConfigFlags *flag.WarcWriterConfigFlags
	WarcRecordOptionFlags flag.WarcRecordOptionFlags
	OutputHookFlags       *flag.OutputHookFlags
	InputHookFlags        *flag.InputHookFlags
	UtilFlags             flag.UtilFlags
	ConcurrencyFlags      flag.ConcurrencyFlags
	ErrorFlags            flag.ErrorFlags
}

func NewConvertArcFlags() ConvertArcFlags {
	return ConvertArcFlags{
		OutputHookFlags:       &flag.OutputHookFlags{},
		InputHookFlags:        &flag.InputHookFlags{},
		WarcWriterConfigFlags: &flag.WarcWriterConfigFlags{},
	}
}

func (f ConvertArcFlags) AddFlags(cmd *cobra.Command) {
	f.FileWalkerFlags.AddFlags(cmd, flag.WithDefaultSuffixes([]string{".arc", ".arc.gz"}))
	f.OutputHookFlags.AddFlags(cmd)
	f.InputHookFlags.AddFlags(cmd)
	f.WarcWriterConfigFlags.AddFlags(cmd, flag.WithDefaultOneToOne(true))
	f.WarcRecordOptionFlags.AddFlags(cmd)
	f.IndexFlags.AddFlags(cmd)
	f.UtilFlags.AddFlags(cmd)
	f.ConcurrencyFlags.AddFlags(cmd)
	f.ErrorFlags.AddFlags(cmd)
}

func (f ConvertArcFlags) ToConvertArcOptions() (_ *ConvertArcOptions, err error) {
	openInputFileHook, err := f.InputHookFlags.ToOpenInputFileHook()
	if err != nil {
		return nil, fmt.Errorf("failed to create open input file hook: %w", err)
	}

	closeInputFileHook, err := f.InputHookFlags.ToCloseInputFileHook()
	if err != nil {
		return nil, fmt.Errorf("failed to create close input file hook: %w", err)
	}

	// Read additional input paths from --source-file-list.
	fileList, err := flag.ReadSrcFileList(viper.GetString(flag.SrcFileList))
	if err != nil {
		return nil, fmt.Errorf("failed to read from source file: %w", err)
	}

	o := &ConvertArcOptions{
		WarcRecordOptions:  f.WarcRecordOptionFlags.ToWarcRecordOptions(),
		Concurrency:        f.ConcurrencyFlags.Concurrency(),
		OpenInputFileHook:  openInputFileHook,
		CloseInputFileHook: closeInputFileHook,
		Paths:              fileList,
		ContinueOnError:    f.ErrorFlags.ContinueOnError(),
		MinWARCDiskFree:    f.UtilFlags.MinFreeDisk(),
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
	if warcWriterConfig.OneToOneWriter {
		warcInfoFunc := func(recordBuilder gowarc.WarcRecordBuilder) error {
			payload := &gowarc.WarcFields{}
			payload.Set("software", version.SoftwareVersion())
			payload.Set("format", fmt.Sprintf("WARC File Format %d.%d", warcWriterConfig.WarcVersion.Major(), warcWriterConfig.WarcVersion.Minor()))
			payload.Set("description", "Converted from ARC")
			hostname, errInner := os.Hostname()
			if errInner != nil {
				return errInner
			}
			payload.Set("host", hostname)

			_, err := recordBuilder.WriteString(payload.String())
			return err
		}
		warcWriterConfig.WarcFileWriterOptions = append(warcWriterConfig.WarcFileWriterOptions, gowarc.WithWarcInfoFunc(warcInfoFunc))
	}

	o.WarcRecordOptions = append(o.WarcRecordOptions,
		gowarc.WithVersion(warcWriterConfig.WarcVersion),
		gowarc.WithAddMissingDigest(true),
	)

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

func NewCmdConvertArc() *cobra.Command {
	flags := NewConvertArcFlags()

	var cmd = &cobra.Command{
		Use:   "arc FILE/DIR ...",
		Short: "Convert ARC to WARC",
		Long:  ``,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			o, err := flags.ToConvertArcOptions()
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

	flags.AddFlags(cmd)

	return cmd
}

func (o *ConvertArcOptions) Complete(cmd *cobra.Command, args []string) error {
	o.Paths = append(o.Paths, args...)
	return nil
}

func (o *ConvertArcOptions) Validate() error {
	if len(o.Paths) == 0 {
		return errors.New("missing file or directory name")
	}
	return nil
}

// Run processes the inputs and waits for workers to finish.
// The caller must close the options after Run returns.
func (o *ConvertArcOptions) Run() error {
	runner := conversion.RunOptions{
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

func (o *ConvertArcOptions) handleFile(ctx context.Context, fs afero.Fs, fileName string, getWriter conversion.GetWriter) (result stat.Result, resultErr error) {
	result = stat.NewResult(fileName)

	arcFileReader, err := arcreader.NewArcFileReader(fs, fileName, 0, o.WarcRecordOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to create arc file reader: %w", err)
	}
	defer func() { _ = arcFileReader.Close() }()

	var writer *gowarc.WarcFileWriter

	records := warc.Compose(arcFileReader.Records(), nil, 0, 0)
	for record, err := range records {
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}
		if writer == nil || !o.WarcWriterConfig.OneToOneWriter {
			warcDate, err := record.WarcRecord.WarcHeader().GetTime(gowarc.WarcDate)
			if err != nil {
				return result, errors.Join(warc.ErrorFrom(record, err), record.Close())
			}
			writer, err = getWriter(fileName, warcDate)
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

func (o *ConvertArcOptions) handleRecord(warcFileWriter *gowarc.WarcFileWriter, record gowarc.Record, result stat.Result) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, record.Close()) }()

	result.IncrRecords()

	if len(record.Validation) > 0 {
		for _, err := range record.Validation {
			result.AddError(warc.ErrorFrom(record, err))
		}
	}

	writeResponse := warcFileWriter.Write(record.WarcRecord)
	if len(writeResponse) > 0 {
		return writeResponse[0].Err
	}
	return nil
}
