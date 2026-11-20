// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package nedlib

import (
	"errors"
	"fmt"
	"os"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/convert/internal/conversion"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/version"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warc"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcwriterconfig"
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
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			o, err := flags.ToOptions()
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
func (o *ConvertNedlibOptions) Run() error {
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

func (o *ConvertNedlibOptions) handleFile(fs afero.Fs, fileName string, getWriter conversion.GetWriter) (result stat.Result, resultErr error) {
	result = stat.NewResult(fileName)

	nedlibReader, err := nedlibreader.NewNedlibReader(fs, fileName, o.WarcWriterConfig.DefaultTime, o.WarcRecordOptions...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = nedlibReader.Close() }()

	var writer *gowarc.WarcFileWriter

	records := warc.Compose(nedlibReader.Records(), nil, 0, 0)
	for record, err := range records {
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}

		warcDate, err := record.WarcRecord.WarcHeader().GetTime(gowarc.WarcDate)
		if err != nil {
			return result, errors.Join(warc.ErrorFrom(record, err), record.Close())
		}

		syntheticFileName := timestamp.Format14(warcDate)

		if o.WarcWriterConfig.OneToOneWriter {
			syntheticFileName = fileName
		}
		if writer == nil || !o.WarcWriterConfig.OneToOneWriter {
			writer, err = getWriter(syntheticFileName, warcDate)
		}
		if err != nil {
			return result, errors.Join(warc.ErrorFrom(record, err), record.Close())
		}

		err = o.handleRecord(writer, record, result)
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}
	}

	return result, nil
}

func (o *ConvertNedlibOptions) handleRecord(w *gowarc.WarcFileWriter, record gowarc.Record, result stat.Result) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, record.Close()) }()

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
