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
	"sync"
	"syscall"
	"time"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/index/reset"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filter"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fulltext"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warc"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/workerpool"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
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
	cmd.AddCommand(reset.NewCmdReset())
	addMaintenanceCommands(cmd)
	return cmd
}

type IndexOptions struct {
	Fulltext, TextRebuild  bool
	TextDir                string
	format, output, tmpDir string
	WaczIndex              string
	WaczConfig             string
	sort                   bool
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
	o.Paths = append(o.Paths, args...)
	o.stdout = cmd.OutOrStdout()
	for _, name := range []string{"id", "record-type", "mime-type", "response-code", "offset", "nth", "limit", flag.StrictValidation, flag.LenientValidation, flag.LaxHostParsing, "force", "post-append"} {
		if flag.Explicit(cmd, name) {
			o.WaczIndex = "warc"
			break
		}
	}
	return nil
}

func (o *IndexOptions) Validate() error {
	if o.WaczIndex != "" && o.WaczIndex != "auto" && o.WaczIndex != "warc" {
		return errors.New("wacz-index must be auto or warc")
	}
	if o.TextRebuild && !o.Fulltext {
		return errors.New("fulltext-rebuild requires fulltext")
	}
	if o.Fulltext && o.format != "badger" {
		return errors.New("fulltext requires index-format badger")
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
	if o.FileWalker != nil {
		err = errors.Join(err, o.FileWalker.Close())
		o.FileWalker = nil
	}
	return err
}

func (o *IndexOptions) Run(ctx context.Context) error {
	if o.Sink == nil {
		if err := o.openSink(ctx); err != nil {
			return err
		}
	}
	if o.Fulltext {
		db, ok := o.Sink.(*cdxstore.DB)
		if !ok || db.IndexOnly() {
			return errors.New("fulltext requires a native index database")
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var failures error
	fail := func(e error) {
		mu.Lock()
		failures = errors.Join(failures, e)
		mu.Unlock()
		if !o.ContinueOnError {
			cancel()
		}
	}
	o.FileWalker.ArchiveHandler = o.handleWACZ
	pool := workerpool.New(ctx, o.Concurrency)
	for _, path := range o.Paths {
		e := o.FileWalker.Walk(ctx, path, func(fs afero.Fs, path string, e error) error {
			if e != nil {
				if o.ContinueOnError && ctx.Err() == nil {
					fail(fmt.Errorf("%s: %w", path, e))
					return nil
				}
				return e
			}
			return pool.Submit(func() {
				e := o.indexInput(ctx, fs, path)
				if e != nil && !errors.Is(e, hooks.ErrSkipFile) {
					fail(fmt.Errorf("%s: %w", path, e))
				}
			})
		})
		if e != nil {
			fail(e)
			if !o.ContinueOnError {
				break
			}
		}
	}
	pool.CloseWait()
	if failures != nil {
		return failures
	}
	if e := ctx.Err(); e != nil {
		return e
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
	case "cdxj", "cdxpb":
		o.Sink, err = cdx.NewOutput(o.stdout, o.output, o.format, o.tmpDir, o.sort)
	case "badger":
		o.Sink, err = cdxstore.Open(ctx, o.database)
	default:
		return fmt.Errorf("unknown index format %q", o.format)
	}
	o.OwnSink = err == nil
	return err
}

type fileFinisher interface {
	BeginFile(context.Context, string) (context.Context, error)
	FinishFile(context.Context, string, int64, time.Time) error
}

type stagedFile struct {
	db       *cdxstore.DB
	ctx      context.Context
	source   string
	size     int64
	modified time.Time
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
		return stage.db.FinishFile(stage.ctx, stage.source, stage.size, stage.modified)
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
	if f, ok := o.Sink.(fileFinisher); ok {
		ctx, err = f.BeginFile(ctx, source)
		if stage != nil {
			stage.ctx, stage.source = ctx, source
		}
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

	var pending *cdx.Capture
	pendingAccept := false
	pendingMethod, pendingQuery := "", ""
	var accepted, written int
	emit := func(c *cdx.Capture, accept bool) error {
		if c == nil || !accept {
			return nil
		}
		accepted++
		if o.Nth > 0 && accepted != o.Nth {
			return nil
		}
		if err := o.Sink.WriteCapture(ctx, c); err != nil {
			return err
		}
		written++
		return nil
	}
	done := func() bool { return (o.Nth > 0 && written > 0) || (o.Limit > 0 && written >= o.Limit) }
	lastOffset := int64(-1)
	coverage := warc.Coverage(o.Offset)
	for !done() {
		rec, e := reader.Next()
		if errors.Is(e, io.EOF) {
			if !o.Force {
				if err := coverage.Finish(positional, info.Size()); err != nil {
					return result, err
				}
			}
			break
		}
		if ctx.Err() != nil {
			_ = rec.Close()
			return result, ctx.Err()
		}
		if e != nil {
			_ = rec.Close()
			if o.Force && lastOffset != rec.Offset {
				lastOffset = rec.Offset
				slog.Warn("Skipping malformed record", "path", path, "offset", rec.Offset, "error", e)
				continue
			}
			return result, warc.ErrorFrom(rec, e)
		}
		if !o.Force {
			if err := coverage.Add(positional, rec); err != nil {
				return result, errors.Join(err, rec.Close())
			}
		}
		result.IncrRecords()
		c, accept, method, query, e := o.captureRecord(positional, rec, source, filename)
		if e != nil {
			return result, e
		}
		paired := o.PostAppend && pair(pending, c, pendingMethod, pendingQuery, method, query)

		if e = emit(pending, pendingAccept); e != nil {
			return result, e
		}
		pending = nil
		if done() {
			break
		}
		pending = c
		pendingAccept = accept
		pendingMethod, pendingQuery = method, query
		if paired || !o.PostAppend || (c.GetSrt() != "request" && c.GetSrt() != "response") {
			if e = emit(pending, pendingAccept); e != nil {
				return result, e
			}
			pending = nil
		}
	}
	if e := emit(pending, pendingAccept); e != nil {
		return result, e
	}
	closed = true
	if e := reader.Close(); e != nil {
		return result, e
	}
	if stage != nil {
		stage.size, stage.modified = info.Size(), info.ModTime()
	} else if f, ok := o.Sink.(fileFinisher); ok {
		if e := o.Sink.Flush(ctx); e != nil {
			return result, e
		}
		if e := f.FinishFile(ctx, source, info.Size(), info.ModTime()); e != nil {
			return result, e
		}
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
	if previous.GetSrt() == "request" && current.GetSrt() == "response" {
		cdx.Augment(current, previousMethod, previousQuery)
		previous.Ssu = current.GetSsu()
		return true
	}
	if current.GetSrt() == "request" && previous.GetSrt() == "response" {
		cdx.Augment(previous, method, query)
		current.Ssu = previous.GetSsu()
		return true
	}
	return false
}
