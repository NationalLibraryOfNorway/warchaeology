// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package dedup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/bytesize"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/diskspace"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warc"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcdigest"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcwriterconfig"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/workerpool"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const badgerRecommendedMaxFileDescr = 65535

const (
	BufferMaxMem     = "max-buffer-mem"
	BufferMaxMemHelp = "the maximum bytes of memory allowed for each buffer before overflowing to disk"

	DedupSizeGain     = "min-size-gain"
	DedupSizeGainHelp = `minimum bytes one must earn to perform a deduplication`

	MinIndexDiskFree     = "min-index-disk-free"
	MinIndexDiskFreeHelp = `minimum free space on disk to allow index writing`

	RecordTypes     = "record-types"
	RecordTypesHelp = `comma separated list of record types to deduplicate. Other record types are written as is.`

	Deterministic     = "deterministic"
	DeterministicHelp = `force deterministic execution order (single worker and sorted input paths)`
)

type DedupOptions struct {
	runStarted, runSucceeded bool
	repair                   bool
	Paths                    []string
	Concurrency              int
	DigestIndex              *index.DigestIndex
	FileIndex                *index.FileIndex
	WarcWriterConfig         *warcwriterconfig.WarcWriterConfig
	MinimumSizeGain          int64
	MinWARCDiskFree          int64
	MinIndexDiskFree         int64
	ContinueOnError          bool
	RecordTypes              []gowarc.RecordType
	FileWalker               *filewalker.FileWalker
	WarcRecordOptions        []gowarc.WarcRecordOption
	Deterministic            bool
	OpenInputFileHook        hooks.OpenInputFileHook
	CloseInputFileHook       hooks.CloseInputFileHook
	OpenOutputFileHook       hooks.OpenOutputFileHook
	CloseOutputFileHook      hooks.CloseOutputFileHook
}

// Close releases the resources owned by the options. Call it after Run returns.
// Repeated calls are safe; Close must not run concurrently with Run or itself.
func (o *DedupOptions) Close() error {
	var err error
	if o.WarcWriterConfig != nil {
		err = errors.Join(err, o.WarcWriterConfig.Close())
		o.WarcWriterConfig = nil
	}
	if o.DigestIndex != nil {
		if o.runStarted && o.runSucceeded && err == nil {
			err = errors.Join(err, o.DigestIndex.CommitOutputRun())
		}
		err = errors.Join(err, o.DigestIndex.Close())
		o.DigestIndex = nil
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

type DedupFlags struct {
	WarcFileFlags         flag.WarcIteratorFlags
	InputHookFlags        *flag.InputHookFlags
	OutputHookFlags       *flag.OutputHookFlags
	FileWalkerFlags       flag.FileWalkerFlags
	WarcWriterConfigFlags *flag.WarcWriterConfigFlags
	UtilFlags             flag.UtilFlags
	RepairFlags           flag.RepairFlags
	WarcRecordOptionFlags flag.WarcRecordOptionFlags
	IndexFlags            flag.IndexFlags
	ConcurrencyFlags      flag.ConcurrencyFlags
	ErrorFlags            flag.ErrorFlags
}

func NewDedupFlags() DedupFlags {
	return DedupFlags{
		InputHookFlags:        &flag.InputHookFlags{},
		OutputHookFlags:       &flag.OutputHookFlags{},
		WarcWriterConfigFlags: &flag.WarcWriterConfigFlags{},
	}
}

func (f DedupFlags) AddFlags(cmd *cobra.Command) {
	f.WarcFileFlags.AddFlags(cmd)
	f.InputHookFlags.AddFlags(cmd)
	f.OutputHookFlags.AddFlags(cmd)
	f.FileWalkerFlags.AddFlags(cmd)
	f.WarcWriterConfigFlags.AddFlags(cmd)
	f.UtilFlags.AddFlags(cmd)
	f.RepairFlags.AddFlags(cmd)
	f.WarcRecordOptionFlags.AddFlags(cmd)
	f.IndexFlags.AddFlags(cmd)
	f.ConcurrencyFlags.AddFlags(cmd)
	f.ErrorFlags.AddFlags(cmd)

	flags := cmd.Flags()
	flags.String(BufferMaxMem, "1MB", BufferMaxMemHelp)
	flags.StringP(DedupSizeGain, "g", "2KB", DedupSizeGainHelp)
	flags.String(MinIndexDiskFree, "1MB", MinIndexDiskFreeHelp)
	flags.StringSlice(RecordTypes, []string{"response", "resource"}, RecordTypesHelp)
	flags.Bool(Deterministic, false, DeterministicHelp)
}

func (f DedupFlags) BufferMaxMem() int64 {
	return bytesize.Parse(viper.GetString(BufferMaxMem))
}

func (f DedupFlags) DedupSizeGain() int64 {
	return bytesize.Parse(viper.GetString(DedupSizeGain))
}

func (f DedupFlags) MinIndexDiskFree() int64 {
	return bytesize.Parse(viper.GetString(MinIndexDiskFree))
}

func (f DedupFlags) RecordTypes() []string {
	return viper.GetStringSlice(RecordTypes)
}

func (f DedupFlags) Deterministic() bool {
	return viper.GetBool(Deterministic)
}

func (f DedupFlags) ToDedupOptions() (_ *DedupOptions, err error) {
	var recordTypes []gowarc.RecordType
	for _, rt := range f.RecordTypes() {
		recordType := stringToRecordType(rt)
		if recordType == gowarc.Revisit {
			return nil, fmt.Errorf("revisit records cannot be deduplicated")
		}
		if recordType == 0 {
			return nil, fmt.Errorf("invalid record type: %s", rt)
		}
		recordTypes = append(recordTypes, stringToRecordType(rt))
	}

	concurrency := f.ConcurrencyFlags.Concurrency()
	deterministic := f.Deterministic()
	if deterministic {
		concurrency = 1
	}

	warcRecordOptions := []gowarc.WarcRecordOption{
		gowarc.WithBufferMaxMemBytes(f.BufferMaxMem()),
	}
	warcRecordOptions = append(warcRecordOptions, f.RepairFlags.ToWarcRecordOptions()...)
	warcRecordOptions = append(warcRecordOptions, f.WarcRecordOptionFlags.ToWarcRecordOptions()...)

	if f.RepairFlags.Repair() {
		// gowarc uses the block digest to fill both resource headers and can
		// overwrite an existing payload label. Preserve it for verification.
		warcRecordOptions = append(warcRecordOptions, gowarc.WithAddMissingDigest(false), gowarc.WithFixDigest(false))
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

	o := &DedupOptions{
		Paths:              fileList,
		repair:             f.RepairFlags.Repair(),
		Concurrency:        concurrency,
		MinimumSizeGain:    f.DedupSizeGain(),
		MinWARCDiskFree:    f.UtilFlags.MinFreeDisk(),
		MinIndexDiskFree:   f.MinIndexDiskFree(),
		ContinueOnError:    f.ErrorFlags.ContinueOnError(),
		RecordTypes:        recordTypes,
		WarcRecordOptions:  warcRecordOptions,
		Deterministic:      deterministic,
		OpenInputFileHook:  openInputFileHook,
		CloseInputFileHook: closeInputFileHook,
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, o.Close())
		}
	}()

	o.WarcWriterConfig, err = f.WarcWriterConfigFlags.ToWarcWriterConfig()
	if err != nil {
		return nil, err
	}

	o.DigestIndex, err = f.IndexFlags.ToDigestIndex()
	if err != nil {
		return nil, fmt.Errorf("failed to create digest index: %w", err)
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

func NewCmdDedup() *cobra.Command {
	flags := NewDedupFlags()

	var cmd = &cobra.Command{
		Use:   "dedup FILE/DIR ...",
		Short: "Deduplicate WARC files",
		Long: `Deduplicate WARC files.

Only selected records with verified payload digests are eligible for deduplication.
Use warc audit COLLECTION to verify revisit targets in existing output.`,
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			o, err := flags.ToDedupOptions()
			if err != nil {
				return err
			}
			defer func() {
				runErr = errors.Join(runErr, o.Close())
			}()
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

func (o *DedupOptions) Complete(cmd *cobra.Command, args []string) error {
	o.Paths = append(o.Paths, args...)
	if o.Deterministic {
		sort.Strings(o.Paths)
	}

	return nil
}

func (o *DedupOptions) Validate() error {
	if o.WarcWriterConfig.OutDir == "" {
		return errors.New("missing output directory")
	}
	if len(o.Paths) == 0 {
		return errors.New("missing file or directory name")
	}
	if len(o.RecordTypes) == 0 {
		return errors.New("missing record type(s)")
	}
	return nil
}

type fileResult struct {
	path   string
	result stat.Result
	source string
}

// Run processes the inputs and publishes the output before recording progress.
// The caller must close the options after Run returns.
func (o *DedupOptions) Run() error {
	if err := o.DigestIndex.BeginOutputRun(); err != nil {
		return err
	}
	o.runStarted = true
	o.WarcWriterConfig.WarcFileWriterOptions = append(o.WarcWriterConfig.WarcFileWriterOptions, gowarc.WithFlush(true))
	done := make(chan struct{})
	var processingErr error
	var processed []fileResult

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	results := make(chan fileResult)
	go func() {
		defer close(done)

		stats := stat.NewStats()
		defer func() {
			slog.Info("Total", "errors", stats.Errors, "files", stats.Files, "records", stats.Records, "duplicates", stats.Duplicates)
		}()

		for item := range results {
			result := item.result
			slog := slog.With("path", item.path)
			for _, err := range result.Errors() {
				if processingErr == nil {
					processingErr = fmt.Errorf("%s: %w", item.path, err)
				}
				if recordErr, ok := errors.AsType[warc.RecordError](err); ok {
					slog.Error("Validation error", "error", recordErr.Error(), "offset", recordErr.Offset())
				} else {
					slog.Error("Validation error", "error", err.Error())
				}
			}
			slog.Info("Deduplicated file", "errors", result.ErrorCount(), "records", result.Records(), "duplicates", result.Duplicates())
			stats.Merge(result)
			if item.source != "" {
				processed = append(processed, item)
			}
		}
		if stats.Errors > 0 && processingErr == nil {
			processingErr = errors.New("processing completed with errors")
		}
	}()

	// Stop scheduling on an input failure while already-started inputs finish.
	walkCtx, stopWalk := context.WithCancel(ctx)
	defer stopWalk()
	workerPool := workerpool.New(walkCtx, o.Concurrency)
	o.FileWalker.WaitForReaders = workerPool.Wait

	// Warn when file descriptor limit is below recommended value
	err := checkFileDescriptorLimit(badgerRecommendedMaxFileDescr)
	if err != nil {
		slog.Warn(err.Error())
	}

	walkErr := o.FileWalker.WalkInputs(walkCtx, o.Paths, o.ContinueOnError, func(fs afero.Fs, path string) error {
		return workerPool.Submit(func() {
			result, err := o.processFile(ctx, fs, path)
			if errors.Is(err, filewalker.ErrSkipFile) {
				return
			}
			if err != nil {
				if !o.ContinueOnError {
					stopWalk()
				}
				if result.result == nil {
					result.result = stat.NewResult(path)
				}
				result.result.AddError(err)
			}

			result.path = path
			results <- result
		})
	})
	workerPool.CloseWait()
	close(results)
	<-done
	if err := errors.Join(walkErr, processingErr, o.WarcWriterConfig.Publish(), ctx.Err()); err != nil {
		return err
	}
	for _, result := range processed {
		if err := o.FileIndex.SaveFileStats(result.source, result.result); err != nil {
			return err
		}
	}
	if len(processed) > 0 {
		if err := o.FileIndex.Sync(); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o.runSucceeded = true
	return nil
}

func (o *DedupOptions) processFile(ctx context.Context, fs afero.Fs, path string) (fileResult, error) {
	var source string
	if o.FileIndex != nil {
		var err error
		source, err = filewalker.SourceLocator(fs, path)
		if err != nil {
			return fileResult{}, err
		}
		cached, err := o.FileIndex.GetFileStats(source)
		if err != nil || cached != nil {
			return fileResult{result: cached}, err
		}
	}
	if err := checkDiskSpace(o.WarcWriterConfig.OutDir, o.MinWARCDiskFree); err != nil {
		return fileResult{}, err
	}
	if err := checkDiskSpace(o.DigestIndex.GetDir(), o.MinIndexDiskFree); err != nil {
		return fileResult{}, err
	}
	result, err := o.processInput(ctx, fs, path)
	return fileResult{result: result, source: source}, err
}

func checkDiskSpace(path string, minimum int64) error {
	if minimum <= 0 {
		return nil
	}
	free, err := diskspace.Free(path)
	if err != nil {
		return fmt.Errorf("get free space at %q: %w", path, err)
	}
	if free < minimum {
		return fmt.Errorf("not enough free space at %q: %d bytes available, %d required", path, free, minimum)
	}
	return nil
}

func (o *DedupOptions) handleFile(fs afero.Fs, path string) (result stat.Result, err error) {
	return o.processInput(context.Background(), fs, path)
}

type donors interface {
	WriteWithDonor(string, *gowarc.RevisitRef, func(*gowarc.RevisitRef) error) error
}

func (o *DedupOptions) processInput(ctx context.Context, fs afero.Fs, path string) (stat.Result, error) {
	var finish func(bool) error
	var registry donors = o.DigestIndex
	var staged *index.FileDonors
	if o.WarcWriterConfig.OneToOneWriter {
		staged = o.DigestIndex.NewFileDonors()
		registry = staged
	}
	result, err := filewalker.PrePostHookContext(ctx, fs, path, o.OpenInputFileHook, o.CloseInputFileHook, func(fs afero.Fs, path string) (stat.Result, error) {
		return o.readFile(ctx, fs, path, registry, func(date time.Time) (*gowarc.WarcFileWriter, error) {
			writer, done, e := o.WarcWriterConfig.WriterForInput(ctx, path, date)
			finish = done
			return writer, e
		})
	})
	err = errors.Join(err, ctx.Err())
	if finish != nil {
		err = errors.Join(err, finish(err == nil))
	}
	if staged != nil {
		err = errors.Join(err, staged.Finish(err == nil))
	}
	return result, err
}
func (o *DedupOptions) readFile(ctx context.Context, fs afero.Fs, path string, registry donors, getWriter func(time.Time) (*gowarc.WarcFileWriter, error)) (result stat.Result, err error) {
	result = stat.NewResult(path)

	file, err := fs.Open(path)
	if err != nil {
		return nil, err
	}

	warcReader, err := gowarc.NewWarcFileReaderFromStream(file, 0, o.WarcRecordOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to create warc file reader: %w", errors.Join(err, file.Close()))
	}
	defer func() { err = errors.Join(err, warcReader.Close()) }()

	var writer *gowarc.WarcFileWriter

	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	var coverage warc.Coverage
	records := warc.Compose(warcReader.Records(), nil, 0, 0)
	for record, err := range records {
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}

		if e := ctx.Err(); e != nil {
			return result, errors.Join(e, record.Close())
		}
		if e := coverage.Add(file, record); e != nil {
			return result, errors.Join(e, record.Close())
		}
		if writer == nil || !o.WarcWriterConfig.OneToOneWriter {
			warcDate, err := record.WarcRecord.WarcHeader().GetTime(gowarc.WarcDate)
			if err != nil {
				return result, errors.Join(warc.ErrorFrom(record, err), record.Close())
			}
			writer, err = getWriter(warcDate)
			if err != nil {
				return result, errors.Join(warc.ErrorFrom(record, err), record.Close())
			}
		}
		err = o.handleRecord(registry, writer, record, result)
		if err != nil {
			return result, warc.ErrorFrom(record, err)
		}
	}

	return result, coverage.Finish(file, info.Size())
}

func (o *DedupOptions) handleRecord(registry donors, writer *gowarc.WarcFileWriter, record gowarc.Record, result stat.Result) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, record.Close()) }()

	result.IncrRecords()

	warcRecord := record.WarcRecord
	if o.repair {
		if err := addMissingDigests(warcRecord); err != nil {
			return err
		}
	}

	// if the record type is not in the list of record types to deduplicate, write the record as is
	if !slices.Contains(o.RecordTypes, warcRecord.Type()) {
		return writeRecord(writer, warcRecord)
	}

	if len(record.Validation) > 0 {
		for _, err := range record.Validation {
			result.AddError(warc.ErrorFrom(record, err))
		}
		return writeRecord(writer, warcRecord)
	}
	digest, err := getDigest(warcRecord)
	if errors.Is(err, warcdigest.ErrRead) {
		return err
	}
	if err != nil {
		result.AddError(warc.ErrorFrom(record, err))
	}
	if digest == "" {
		return writeRecord(writer, warcRecord)
	}
	profile := getRevisitProfile(warcRecord)

	// Write response if no profile is found (can't make a revisit record without a profile)
	if profile == "" {
		return writeRecord(writer, warcRecord)
	}

	revisitRef, err := warcRecord.CreateRevisitRef(profile)
	if err != nil {
		result.AddError(warc.ErrorFrom(record, fmt.Errorf("error creating revisit ref: %w", err)))
	}

	if err != nil {
		return writeRecord(writer, warcRecord)
	}
	return registry.WriteWithDonor(digest, revisitRef, func(revisitReference *gowarc.RevisitRef) error {
		// Overlapping input files can contain copies of the same record.
		if revisitReference == nil || (revisitRef.TargetRecordId != "" && revisitReference.TargetRecordId == revisitRef.TargetRecordId) {
			return writeRecord(writer, warcRecord)
		}
		if int64(revisitRefSize(revisitReference)) >= payloadLength(warcRecord)-o.MinimumSizeGain {
			return writeRecord(writer, warcRecord)
		}
		revisit, err := warcRecord.ToRevisitRecord(revisitReference)
		if err != nil {
			result.AddError(warc.ErrorFrom(record, err))
			return writeRecord(writer, warcRecord)
		}
		defer func() { resultErr = errors.Join(resultErr, revisit.Close()) }()
		err = writeRecord(writer, revisit)
		if err == nil {
			result.IncrDuplicates()
		}
		return err
	})
}

func writeRecord(writer *gowarc.WarcFileWriter, warcRecord gowarc.WarcRecord) error {
	writeResponse := writer.Write(warcRecord)
	if len(writeResponse) > 0 {
		return writeResponse[0].Err
	}
	return nil
}

// getRevisitProfile returns the revisit profile for a record
func getRevisitProfile(warcRecord gowarc.WarcRecord) string {
	switch warcRecord.Version() {
	case gowarc.V1_0:
		return gowarc.ProfileIdenticalPayloadDigestV1_0
	case gowarc.V1_1:
		return gowarc.ProfileIdenticalPayloadDigestV1_1
	default:
		return ""
	}
}

func addMissingDigests(r gowarc.WarcRecord) error {
	h := r.WarcHeader()
	if h.Has(gowarc.WarcBlockDigest) && h.Has(gowarc.WarcPayloadDigest) {
		return nil
	}
	if err := r.Block().Cache(); err != nil {
		return err
	}
	if !h.Has(gowarc.WarcBlockDigest) {
		h.Set(gowarc.WarcBlockDigest, r.Block().BlockDigest())
	}
	if h.Has(gowarc.WarcPayloadDigest) || h.Has(gowarc.WarcSegmentNumber) || r.Type() == gowarc.Revisit {
		return nil
	}
	if block, ok := r.Block().(gowarc.PayloadBlock); ok {
		h.Set(gowarc.WarcPayloadDigest, block.PayloadDigest())
	} else if r.Type() == gowarc.Resource {
		h.Set(gowarc.WarcPayloadDigest, r.Block().BlockDigest())
	}
	return nil
}

// getDigest produces a donor key only for a complete, verified payload.
func getDigest(r gowarc.WarcRecord) (string, error) {
	h := r.WarcHeader()
	if h.Has(gowarc.WarcTruncated) {
		return "", nil
	}
	for _, field := range *h {
		if strings.HasPrefix(strings.ToLower(field.Name), "warc-segment-") {
			return "", nil
		}
	}
	label := h.Get(gowarc.WarcPayloadDigest)
	switch r.Type() {
	case gowarc.Response:
		if _, ok := r.Block().(gowarc.HttpResponseBlock); !ok {
			return "", nil
		}
	case gowarc.Resource:
		if label == "" {
			label = h.Get(gowarc.WarcBlockDigest)
		}
	default:
		return "", nil
	}
	if label == "" {
		return "", nil
	}
	if err := r.Block().Cache(); err != nil {
		return "", errors.Join(warcdigest.ErrRead, err)
	}
	if n, err := r.ContentLength(); err != nil || n != r.Block().Size() {
		return "", errors.New("invalid content length")
	}
	if declared := h.Get(gowarc.WarcBlockDigest); declared != "" && !warcdigest.Equal(declared, r.Block().BlockDigest()) {
		return "", errors.New("block digest mismatch")
	}
	switch block := r.Block().(type) {
	case gowarc.PayloadBlock:
		if !warcdigest.Equal(label, block.PayloadDigest()) {
			return "", errors.New("payload digest mismatch")
		}
	default:
		if warcdigest.Equal(label, block.BlockDigest()) {
			break
		}
		raw, err := block.RawBytes()
		if err != nil {
			return "", errors.Join(warcdigest.ErrRead, err)
		}
		if err := warcdigest.Verify(label, raw); err != nil {
			return "", err
		}
	}
	// pywb's local CDX resolver compares the literal labels on the two records.
	if r.Type() == gowarc.Resource && !h.Has(gowarc.WarcPayloadDigest) {
		h.Set(gowarc.WarcPayloadDigest, label)
	}
	return r.Type().String() + "/" + label, nil
}

func payloadLength(r gowarc.WarcRecord) int64 {
	size := r.Block().Size()
	if b, ok := r.Block().(gowarc.ProtocolHeaderBlock); ok {
		size -= int64(len(b.ProtocolHeaderBytes()))
	}
	return size
}

func revisitRefSize(revisitReference *gowarc.RevisitRef) int {
	length := 0
	if revisitReference.TargetRecordId != "" {
		length += len(gowarc.WarcRefersTo) + len(revisitReference.TargetRecordId)
	}
	if revisitReference.Profile != "" {
		length += len(gowarc.WarcProfile) + len(revisitReference.Profile)
	}
	if revisitReference.TargetUri != "" {
		length += len(gowarc.WarcRefersToTargetURI) + len(revisitReference.TargetUri)
	}
	if revisitReference.TargetDate != "" {
		length += len(gowarc.WarcRefersToDate) + len(revisitReference.TargetDate)
	}
	return length
}

func stringToRecordType(rt string) gowarc.RecordType {
	switch rt {
	case "warcinfo":
		return gowarc.Warcinfo
	case "response":
		return gowarc.Response
	case "resource":
		return gowarc.Resource
	case "request":
		return gowarc.Request
	case "metadata":
		return gowarc.Metadata
	case "revisit":
		return gowarc.Revisit
	case "conversion":
		return gowarc.Conversion
	case "continuation":
		return gowarc.Continuation
	default:
		return 0
	}
}
