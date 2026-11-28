// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"archive/zip"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nationallibraryofnorway/gowarc/v4"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/index/reset"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/diskspace"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filter"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fulltext"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warc"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func NewCmdIndex() *cobra.Command {
	flags := NewIndexFlags()
	cmd := &cobra.Command{
		Use:               "index FILE/DIR...",
		Short:             "Index WARC captures",
		ValidArgsFunction: flag.SuffixCompletionFn,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			o, err := flags.ToOptions()
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, o.Close()) }()
			o.pywb = viper.GetBool("pywb")
			if o.format == badgerFormat && flag.Explicit(cmd, flag.IndexFile) {
				return errors.New("index-file requires index-format cdxj or cdxpb")
			}
			if o.format != badgerFormat {
				for _, name := range []string{flag.IndexDir, "badger-database"} {
					if flag.Explicit(cmd, name) {
						return fmt.Errorf("%s requires index-format badger", name)
					}
				}
			}
			if err = o.Complete(cmd, args); err != nil {
				return err
			}
			if err = o.Validate(); err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer cancel()
			cmd.SilenceUsage = true
			return o.Run(ctx)
		},
	}
	flags.AddFlags(cmd)
	cmd.Flags().Bool("pywb", false, "resolve revisit payload locations for offline pywb CDXJ replay")
	cmd.AddCommand(reset.NewCmdReset())
	cmd.AddCommand(newCmdHydrate())
	addMaintenanceCommands(cmd)
	return cmd
}

const badgerFormat = "badger"

type IndexOptions struct {
	Fulltext, TextRebuild  bool
	TextDir                string
	format, output, tmpDir string
	WaczIndex              string
	LongURLKeys            string
	WarnValidation         bool
	WaczConfig             string
	sort                   bool
	pywb                   bool
	pywbDir                string
	stdout                 io.Writer
	database               cdxstore.Options
	Paths                  []string
	Concurrency            int
	Force                  bool
	ContinueOnError        bool
	PostAppend             bool
	DefaultRecords         bool
	OwnSink                bool
	DirRoot                string
	Offset                 int64
	Nth, Limit             int
	FileWalker             *filewalker.FileWalker
	Filter                 *filter.RecordFilter
	OpenInputFileHook      hooks.OpenInputFileHook
	CloseInputFileHook     hooks.CloseInputFileHook
	WarcRecordOptions      []gowarc.WarcRecordOption
	Sink                   cdx.Sink
}

func (o *IndexOptions) Complete(cmd *cobra.Command, args []string) error {
	if err := o.validateWaczIndex(); err != nil {
		return err
	}
	o.Paths = append(o.Paths, args...)
	o.stdout = cmd.OutOrStdout()
	if o.pywb {
		o.WaczIndex = waczRawIndex
	}
	for _, name := range []string{"id", "record-type", "mime-type", "response-code", "offset", "nth", "limit", flag.StrictValidation, flag.LenientValidation, flag.LaxHostParsing, "force", "post-append"} {
		if flag.Explicit(cmd, name) {
			o.WaczIndex = waczRawIndex
			break
		}
	}
	return nil
}

func (o *IndexOptions) validateWaczIndex() error {
	if o.WaczIndex != "" && o.WaczIndex != "auto" && o.WaczIndex != waczRawIndex {
		return errors.New("wacz-index must be auto or warc")
	}
	return nil
}

func (o *IndexOptions) Validate() error {
	if err := o.validateWaczIndex(); err != nil {
		return err
	}
	switch o.LongURLKeys {
	case "", "preserve", "skip", "error":
	default:
		return errors.New("long-url-keys must be preserve, skip, or error")
	}
	if o.TextRebuild && !o.Fulltext {
		return errors.New("fulltext-rebuild requires fulltext")
	}
	if o.Fulltext && o.format != badgerFormat {
		return errors.New("fulltext requires index-format badger")
	}
	if o.pywb && o.format != "cdxj" {
		return errors.New("pywb requires index-format cdxj")
	}
	if len(o.Paths) == 0 && !o.Fulltext {
		return errors.New("missing file or directory name")
	}
	if o.Concurrency <= 0 {
		return errors.New("concurrency must be positive")
	}
	return nil
}

func (o *IndexOptions) Close() error {
	var err error
	if o.OwnSink && o.Sink != nil {
		err = o.Sink.Close()
		o.Sink = nil
	}
	if o.pywbDir != "" {
		err = errors.Join(err, os.RemoveAll(o.pywbDir))
		o.pywbDir = ""
	}
	if o.FileWalker != nil {
		err = errors.Join(err, o.FileWalker.Close())
		o.FileWalker = nil
	}
	return err
}

func (o *IndexOptions) Run(ctx context.Context) error {
	if o.Sink == nil {
		if o.format != badgerFormat && o.output != "-" && o.output != "" {
			if err := o.FileWalker.CheckOutputPaths(ctx, o.Paths, o.output); err != nil {
				return err
			}
		}
		if err := o.openSink(ctx); err != nil {
			return err
		}
	}
	// Apply storage policy only to persistent ingestion. Pywb uses a scratch DB.
	if _, persistent := o.Sink.(*cdxstore.DB); !persistent || o.pywb {
		effective := *o
		effective.LongURLKeys = "preserve"
		o = &effective
	}
	if o.Fulltext {
		db, ok := o.Sink.(*cdxstore.DB)
		if !ok || db.IndexOnly() {
			return errors.New("fulltext requires a native index database")
		}
	}
	o.FileWalker.ArchiveHandler = o.handleWACZ
	err := o.FileWalker.ProcessInputs(ctx, o.Paths, o.Concurrency, o.ContinueOnError, func(ctx context.Context, fs afero.Fs, path string) error {
		err := o.indexInput(ctx, fs, path)
		if errors.Is(err, hooks.ErrSkipFile) && !diskspace.Exhausted(err) {
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if o.pywb {
		return o.exportPywb(ctx, o.Sink.(*cdxstore.DB))
	}
	if err := o.Sink.Flush(ctx); err != nil {
		return err
	}
	if o.Fulltext {
		db := o.Sink.(*cdxstore.DB)
		dir := o.TextDir
		if dir == "" {
			dir = filepath.Join(o.database.Dir, "fulltext")
		}
		idx, err := fulltext.Open(dir)
		if err != nil {
			return err
		}
		syncer := fulltext.Synchronizer{Index: idx, DB: db}
		err = syncer.Sync(ctx, o.TextRebuild)
		return errors.Join(err, idx.Close())
	}
	return nil
}

func (o *IndexOptions) openSink(ctx context.Context) error {
	var err error
	switch o.format {
	case "cdxj":
		if o.pywb {
			o.pywbDir, err = os.MkdirTemp(o.tmpDir, "warc-pywb-*")
			if err != nil {
				return err
			}
			o.Sink, err = cdxstore.Open(ctx, cdxstore.Options{Format: badgerFormat, Dir: o.pywbDir, TempDir: o.pywbDir, Scratch: true})
			break
		}
		fallthrough
	case "cdxpb":
		o.Sink, err = cdx.NewOutput(o.stdout, o.output, o.format, o.tmpDir, o.sort)
	case badgerFormat:
		o.Sink, err = cdxstore.Open(ctx, o.database)
	default:
		return fmt.Errorf("unknown index format %q", o.format)
	}
	o.OwnSink = err == nil
	return err
}

type stagedFile struct {
	db        *cdxstore.DB
	ctx       context.Context
	source    string
	version   filewalker.FileVersion
	container *filewalker.FileVersion
}

func (o *IndexOptions) indexInput(ctx context.Context, fs afero.Fs, path string) (err error) {
	var stage *stagedFile
	if db, ok := o.Sink.(*cdxstore.DB); ok {
		stage = &stagedFile{db: db}
		defer func() {
			if stage.ctx != nil {
				err = errors.Join(err, db.DiscardFile(stage.ctx))
			}
		}()
	}
	_, err = filewalker.PrePostHookContext(ctx, fs, path, o.OpenInputFileHook, o.CloseInputFileHook, func(fs afero.Fs, p string) (stat.Result, error) {
		return o.handleFile(ctx, fs, p, stage)
	})
	if err == nil && stage != nil {
		return stage.db.FinishFileVersion(stage.ctx, stage.source, stage.version, stage.container)
	}
	return err
}

func (o *IndexOptions) handleFile(ctx context.Context, fs afero.Fs, path string, stage *stagedFile) (result stat.Result, err error) {
	result = stat.NewResult(path)
	source, err := filewalker.SourceLocator(fs, path)
	if err != nil {
		return result, err
	}
	filename, err := captureFilename(fs, path, o.DirRoot)
	if err != nil {
		return result, err
	}

	var container *filewalker.FileVersion
	if stage != nil {
		version, present, e := filewalker.ContainerVersion(source)
		if e != nil {
			return result, e
		}
		if present {
			container = &version
		}
		ctx, err = stage.db.BeginFile(ctx, source)
		stage.ctx, stage.source = ctx, source
		if err != nil {
			return result, err
		}
	}
	file, err := fs.Open(path)
	if err != nil {
		return result, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return result, err
	}
	var input io.ReadSeeker = file
	var positional io.ReaderAt = file
	if header, ok := info.Sys().(*zip.FileHeader); ok && header.Method == zip.Deflate {
		defer func() { err = errors.Join(err, file.Close()) }()
		cache, e := os.CreateTemp(o.tmpDir, "warc-member-*")
		if e != nil {
			return result, e
		}
		defer func() { err = errors.Join(err, cache.Close(), os.Remove(cache.Name())) }()
		positional = &inflatedMember{ctx: ctx, source: file, cache: cache}
		input = io.NewSectionReader(positional, 0, info.Size())
	}
	reader, err := gowarc.NewWarcFileReaderFromStream(input, o.Offset, o.WarcRecordOptions...)
	if err != nil {
		if input == file {
			err = errors.Join(err, file.Close())
		}
		return result, err
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, reader.Close())
		}
	}()

	report := longKeyReport{source: source, position: "offset", policy: o.LongURLKeys}
	defer report.summary()
	var pending *cdx.Capture
	pendingAccept := false
	pendingMethod, pendingQuery := "", ""
	var accepted int
	done := func() bool {
		return (o.Nth > 0 && accepted >= o.Nth) || (o.Nth <= 0 && o.Limit > 0 && accepted >= o.Limit)
	}
	emit := func(c *cdx.Capture, accept bool) error {
		if c == nil {
			return nil
		}
		selected := false
		if accept && !done() {
			accepted++
			selected = o.Nth <= 0 || accepted == o.Nth
		}
		keepDonor := o.pywb && canPairWithRequest(c)
		if !selected && !keepDonor {
			return nil
		}
		if o.pywb && selected {
			// Mark rows for export in the scratch catalogue. Unselected
			// response and revisit records still participate in donor lookup.
			c.ExtraJson = []byte("{}")
		}
		if skip, err := report.check(c.GetSsu(), c.GetOffset()); skip || err != nil {
			return err
		}
		if err := o.Sink.WriteCapture(ctx, c); err != nil {
			return fmt.Errorf("offset %d: %w", c.GetOffset(), err)
		}
		return nil
	}
	records := warc.Records(ctx, reader, o.Force, path)
	if !o.Force {
		records = warc.CheckCoverage(records, positional, info.Size(), o.Offset)
	}
	for rec, e := range records {
		if e != nil {
			return result, warc.ErrorFrom(rec, e)
		}
		if o.WarnValidation && len(rec.Validation) > 0 {
			slog.Warn("Indexing record with validation findings", "path", path, "offset", rec.Offset, "error", errors.Join(rec.Validation...))
		}
		result.IncrRecords()
		c, accept, method, query, e := o.captureRecord(positional, rec, source, filename)
		if e != nil {
			return result, warc.ErrorFrom(rec, e)
		}
		paired := o.PostAppend && pair(pending, c, pendingMethod, pendingQuery, method, query)

		if e = emit(pending, pendingAccept); e != nil {
			return result, e
		}
		pending = nil
		if !o.pywb && done() {
			break
		}
		pending = c
		pendingAccept = accept
		pendingMethod, pendingQuery = method, query
		if paired || !o.PostAppend || (c.GetSrt() != "request" && !canPairWithRequest(c)) {
			if e = emit(pending, pendingAccept); e != nil {
				return result, e
			}
			pending = nil
		}
		if !o.pywb && done() {
			break
		}
	}
	if e := emit(pending, pendingAccept); e != nil {
		return result, e
	}
	version := filewalker.VersionOf(info)
	if stage != nil {
		if e := checkArchiveVersion(file, version, source, container); e != nil {
			return result, e
		}
	}
	closed = true
	if e := reader.Close(); e != nil {
		return result, e
	}
	if stage != nil {
		stage.version, stage.container = version, container
	}
	return result, nil
}

func captureFilename(fs afero.Fs, name, root string) (string, error) {
	if root == "" {
		return filepath.Base(name), nil
	}
	if fs.Name() == "OsFs" {
		var err error
		if root, err = filepath.Abs(root); err != nil {
			return "", err
		}
		if name, err = filepath.Abs(name); err != nil {
			return "", err
		}
	}
	relative, err := filepath.Rel(root, name)
	return filepath.ToSlash(relative), err
}

func checkArchiveVersion(file afero.File, version filewalker.FileVersion, source string, container *filewalker.FileVersion) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !version.Matches(filewalker.VersionOf(info)) {
		return errors.New("archive source changed during indexing")
	}
	if container != nil {
		current, present, err := filewalker.ContainerVersion(source)
		if err != nil {
			return err
		}
		if !present || !container.Matches(current) {
			return errors.New("archive container changed during indexing")
		}
	}
	return nil
}

func (o *IndexOptions) captureRecord(file io.ReaderAt, rec gowarc.Record, source, filename string) (c *cdx.Capture, accept bool, method, query string, err error) {
	defer func() { err = errors.Join(err, rec.Close()) }()
	accept = true
	if o.DefaultRecords && (rec.WarcRecord.Type() == gowarc.Resource || rec.WarcRecord.Type() == gowarc.Metadata) && rec.WarcRecord.WarcHeader().Get(gowarc.ContentType) == "application/warc-fields" {
		accept = false
	}
	rec.Size, err = storedLength(file, rec)
	if err != nil {
		return
	}
	c, err = cdx.NewCapture(rec, source, filename)
	if err != nil {
		return
	}
	if o.Filter != nil {
		accept = accept && o.Filter.AcceptCapture(rec.WarcRecord, c.GetMct(), int(c.GetHsc()))
	}
	if o.PostAppend && rec.WarcRecord.Type() == gowarc.Request {
		method, query, err = cdx.RequestMetadata(rec)
	}
	return
}

// inflatedMember retains the inflated prefix for backward framing reads.
type inflatedMember struct {
	ctx    context.Context
	source io.Reader
	cache  *os.File
	size   int64
	buffer [32768]byte
}

func (m *inflatedMember) ReadAt(p []byte, off int64) (int, error) {
	end := off + int64(len(p))
	for m.size < end {
		if err := m.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := m.source.Read(m.buffer[:min(end-m.size, int64(len(m.buffer)))])
		if n > 0 {
			written, e := m.cache.Write(m.buffer[:n])
			m.size += int64(written)
			if e != nil {
				return 0, e
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.ErrNoProgress
		}
	}
	return m.cache.ReadAt(p, off)
}

func storedLength(file io.ReaderAt, rec gowarc.Record) (int64, error) {
	var magic [2]byte
	if _, e := file.ReadAt(magic[:], rec.Offset); e != nil {
		return 0, e
	}
	if magic == [2]byte{0x1f, 0x8b} {
		return rec.Size, nil
	}
	// Match warcio: uncompressed length excludes all framing after the block.
	br := bufio.NewReader(io.NewSectionReader(file, rec.Offset, rec.Size))
	var headers int64
	for {
		line, e := br.ReadString('\n')
		if e != nil {
			return 0, e
		}
		headers += int64(len(line))
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	length, e := rec.WarcRecord.WarcHeader().GetInt64(gowarc.ContentLength)
	if e != nil {
		return 0, e
	}
	return headers + length, nil
}

func pair(previous, current *cdx.Capture, previousMethod, previousQuery, method, query string) bool {
	if previous == nil || previous.GetUri() != current.GetUri() || current.GetRct() != previous.GetRid() {
		return false
	}
	if previous.GetSrt() == "request" && canPairWithRequest(current) {
		cdx.Augment(current, previousMethod, previousQuery)
		previous.Ssu = current.GetSsu()
		return true
	}
	if current.GetSrt() == "request" && canPairWithRequest(previous) {
		cdx.Augment(previous, method, query)
		current.Ssu = previous.GetSsu()
		return true
	}
	return false
}

func canPairWithRequest(c *cdx.Capture) bool {
	return c.GetSrt() == "response" || c.GetSrt() == "revisit"
}
