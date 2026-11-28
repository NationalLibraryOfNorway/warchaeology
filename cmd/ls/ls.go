// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package ls

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nationallibraryofnorway/gowarc/v4"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filter"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warc"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	Delimiter     = "delimiter"
	DelimiterHelp = `field delimiter`

	Fields     = "fields"
	FieldsHelp = `which fields to include in the output

Field specification letters are mostly the same as the fields in the CDX file specification (https://iipc.github.io/warc-specifications/specifications/cdx-format/cdx-2015/).

The following fields are supported:
	a - original URL
	b - date in 14 digit format
	B - date in RFC3339 format
	e - IP address
	g - filename
	h - original host
	i - record id
	k - checksum
	m - document mime type
	s - http response code
	S - record size
	T - record type
	V - offset

A number after the field letter restricts the field length. By adding a + or - sign before the number the field is padded to have the exact length. + is right aligned and - is left aligned.`
)

type Writer interface {
	WriteRecord(record gowarc.Record, fileName string) error
}

type ListOptions struct {
	paths             []string
	offset            int64
	recordNum         int
	recordCount       int
	force             bool
	concurrency       int
	continueOnError   bool
	filter            *filter.RecordFilter
	writer            Writer
	fileWalker        *filewalker.FileWalker
	warcRecordOptions []gowarc.WarcRecordOption
}

// Close releases the resources owned by the options. Call it after Run returns.
// Repeated calls are safe; Close must not run concurrently with Run or itself.
func (o *ListOptions) Close() error {
	var err error
	if o.fileWalker != nil {
		err = errors.Join(err, o.fileWalker.Close())
		o.fileWalker = nil
	}
	return err
}

type ListFlags struct {
	FileWalkerFlags       flag.FileWalkerFlags
	FilterFlags           flag.FilterFlags
	WarcIteratorFlags     flag.WarcIteratorFlags
	WarcRecordOptionFlags flag.WarcRecordOptionFlags
	ConcurrencyFlags      flag.ConcurrencyFlags
	ErrorFlags            flag.ErrorFlags
}

func (f ListFlags) AddFlags(cmd *cobra.Command) {
	f.FileWalkerFlags.AddFlags(cmd)
	f.FilterFlags.AddFlags(cmd)
	f.WarcIteratorFlags.AddFlags(cmd)
	f.WarcRecordOptionFlags.AddFlags(cmd)
	f.ConcurrencyFlags.AddFlags(cmd)
	f.ErrorFlags.AddFlags(cmd)

	flags := cmd.Flags()

	flags.StringP(Delimiter, "d", " ", DelimiterHelp)
	flags.StringP(Fields, "F", "", FieldsHelp)
	flags.Bool("json", false, "output as JSON lines")
}

func (f ListFlags) Delimiter() string {
	return viper.GetString(Delimiter)
}

func (f ListFlags) Fields() string {
	return viper.GetString(Fields)
}

func (f ListFlags) JSON() bool {
	return viper.GetBool("json")
}

func (f ListFlags) ToListOptions() (*ListOptions, error) {
	filter, err := f.FilterFlags.ToFilter()
	if err != nil {
		return nil, err
	}

	paths, err := flag.ReadSrcFileList(f.FileWalkerFlags.SrcFileListFlags.SrcFileList())
	if err != nil {
		return nil, fmt.Errorf("failed to read from source file: %w", err)
	}

	opts := f.WarcRecordOptionFlags.ToWarcRecordOptions()
	if !f.WarcRecordOptionFlags.StrictValidation() && !fieldsNeedParsedBlock(f.Fields()) {
		opts = append(opts, gowarc.WithSkipParseBlock())
	}

	var writer Writer
	if f.JSON() {
		writer = NewJSONWriter(os.Stdout, f.Fields())
	} else {
		writer, err = NewRecordWriter(os.Stdout, f.Fields(), f.Delimiter())
		if err != nil {
			return nil, err
		}
	}

	o := &ListOptions{
		paths:             paths,
		offset:            f.WarcIteratorFlags.Offset(),
		recordNum:         f.WarcIteratorFlags.RecordNum(),
		recordCount:       f.WarcIteratorFlags.Limit(),
		force:             f.WarcIteratorFlags.Force(),
		concurrency:       f.ConcurrencyFlags.Concurrency(),
		filter:            filter,
		continueOnError:   f.ErrorFlags.ContinueOnError(),
		writer:            writer,
		warcRecordOptions: opts,
	}

	o.fileWalker, err = f.FileWalkerFlags.ToFileWalker()
	if err != nil {
		return nil, fmt.Errorf("failed to create file walker: %w", err)
	}

	return o, nil
}

// NewCmdList creates the ls command
func NewCmdList() *cobra.Command {
	flags := ListFlags{}

	cmd := &cobra.Command{
		Use:   "ls FILE/DIR ...",
		Short: "List WARC record fields",
		Long:  `List information about WARC records`,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			o, err := flags.ToListOptions()
			if err != nil {
				return err
			}
			defer func() { runErr = errors.Join(runErr, o.Close()) }()
			if err := o.Complete(cmd, args); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
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

// Complete completes the options
func (o *ListOptions) Complete(cmd *cobra.Command, args []string) error {
	o.paths = append(o.paths, args...)
	return nil
}

// Validate validates the options
func (o *ListOptions) Validate() error {
	if len(o.paths) == 0 {
		return errors.New("missing file or directory name")
	}
	return nil
}

// Run runs the ls command
// The caller must close the options after Run returns.
func (o *ListOptions) Run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	return o.fileWalker.ProcessInputs(ctx, o.paths, o.concurrency, o.continueOnError, o.handleFile)
}

// listFile reads a warc file and writes the records to the output
func (o *ListOptions) handleFile(ctx context.Context, fs afero.Fs, path string) (resultErr error) {
	f, err := fs.Open(path)
	if err != nil {
		return err
	}
	warcFileReader, err := gowarc.NewWarcFileReaderFromStream(f, o.offset, o.warcRecordOptions...)
	if err != nil {
		return errors.Join(err, f.Close())
	}
	defer func() { resultErr = errors.Join(resultErr, warcFileReader.Close()) }()

	raw := warc.Records(ctx, warcFileReader, o.force, path)

	records := warc.Compose(raw, o.filter, o.recordNum, o.recordCount)
	for record, err := range records {
		if ctx.Err() != nil {
			return errors.Join(err, ctx.Err(), record.Close())
		}
		if err != nil {
			return warc.ErrorFrom(record, err)
		}
		if err := o.handleRecord(record, path); err != nil {
			return warc.ErrorFrom(record, err)
		}
	}

	return nil
}

func (o *ListOptions) handleRecord(record gowarc.Record, path string) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, record.Close()) }()
	return o.writer.WriteRecord(record, path)
}
