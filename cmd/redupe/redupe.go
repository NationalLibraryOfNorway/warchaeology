// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

// Package redupe rebuilds time-partitioned WARC collections without discarding captures.
package redupe

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/bytesize"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/publication"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcwriterconfig"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type RedupeFlags struct {
	FileWalkerFlags  flag.FileWalkerFlags
	InputHookFlags   flag.InputHookFlags
	OutputHookFlags  flag.OutputHookFlags
	WriterFlags      flag.WarcWriterConfigFlags
	ConcurrencyFlags flag.ConcurrencyFlags
}

func NewRedupeFlags() *RedupeFlags { return &RedupeFlags{} }

func (f *RedupeFlags) AddFlags(cmd *cobra.Command) {
	f.FileWalkerFlags.AddFlags(cmd)
	f.InputHookFlags.AddFlags(cmd)
	f.OutputHookFlags.AddFlags(cmd)
	f.WriterFlags.AddFlags(cmd)
	f.ConcurrencyFlags.AddFlags(cmd)
	cmd.Flags().String("interval", "year", "group records by UTC year, month or day")
	cmd.Flags().Bool("audit", false, "plan without writing replacement WARCs")
	cmd.Flags().Bool("verify-payloads", false, "verify payload digests during audit (always enabled for rewriting)")
	cmd.Flags().String("report", "", "write the audit or transformation report as JSON")
	cmd.Flags().String("index-dir", "", "local working directory; retained run state enables safe restart")
	cmd.Flags().String("min-size-gain", "2KB", "minimum payload size saving required to emit a revisit")
}

type RedupeOptions struct {
	collectionAudit                 bool
	Interval                        string
	reportWriter                    io.Writer
	Paths                           []string
	Audit, VerifyPayloads           bool
	ReportPath, IndexDir, OutputDir string
	Concurrency                     int
	MinimumSizeGain                 int64
	FileWalker                      *filewalker.FileWalker
	OpenInputFileHook               hooks.OpenInputFileHook
	CloseInputFileHook              hooks.CloseInputFileHook
	WriterConfig                    *warcwriterconfig.WarcWriterConfig
	configuration                   map[string]any
	catalog                         *inventoryCatalog
	scratch                         *badger.DB
	findings                        *os.File
	state                           *badger.DB
	preview                         previewState
	keepInventory                   bool
	workDir                         string
	temporaryIndex                  bool
	report                          Report
}

func (f *RedupeFlags) ToRedupeOptions() (_ *RedupeOptions, err error) {
	o := &RedupeOptions{
		Interval:        viper.GetString("interval"),
		Audit:           viper.GetBool("audit"),
		VerifyPayloads:  viper.GetBool("verify-payloads"),
		ReportPath:      viper.GetString("report"),
		IndexDir:        viper.GetString("index-dir"),
		OutputDir:       f.WriterFlags.OutputDir(),
		Concurrency:     f.ConcurrencyFlags.Concurrency(),
		MinimumSizeGain: bytesize.Parse(viper.GetString("min-size-gain")),
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, o.Close())
		}
	}()
	if err := o.validateInterval(); err != nil {
		return nil, err
	}
	if f.WriterFlags.OneToOne() || f.WriterFlags.NameGenerator() == "identity" {
		return nil, errors.New("redupe cannot map one input file to one output file")
	}
	if f.WriterFlags.NameGenerator() != "default" || f.WriterFlags.SubdirPattern() != "" {
		return nil, errors.New("redupe uses period directories and the default filename generator")
	}
	o.Paths, err = flag.ReadSrcFileList(f.FileWalkerFlags.SrcFileListFlags.SrcFileList())
	if err != nil {
		return nil, err
	}
	o.FileWalker, err = f.FileWalkerFlags.ToFileWalker()
	if err != nil {
		return nil, err
	}
	o.OpenInputFileHook, err = f.InputHookFlags.ToOpenInputFileHook()
	if err != nil {
		return nil, err
	}
	o.CloseInputFileHook, err = f.InputHookFlags.ToCloseInputFileHook()
	if err != nil {
		return nil, err
	}
	if !o.Audit {
		if err = os.MkdirAll(o.OutputDir, 0755); err != nil {
			return nil, err
		}
		o.WriterConfig, err = f.WriterFlags.ToWarcWriterConfig()
		if err != nil {
			return nil, err
		}
	}
	o.configuration = map[string]any{
		"recursive": o.FileWalker.Recursive, "symlinks": o.FileWalker.FollowSymlinks, "suffixes": o.FileWalker.Suffixes,
		"input_hook": f.InputHookFlags.OpenInputFileHook(), "close_input_hook": f.InputHookFlags.CloseInputFileHook(),
		"policy":             "utc-period-preserve-all-v2",
		"min_size_gain":      o.MinimumSizeGain,
		"compress":           f.WriterFlags.Compress(),
		"compression_level":  f.WriterFlags.CompressionLevel(),
		"file_size":          f.WriterFlags.FileSize(),
		"prefix":             f.WriterFlags.FilePrefix(),
		"warc_version":       f.WriterFlags.WarcVersion(),
		"concurrent_writers": f.WriterFlags.ConcurrentWriters(),
		"flush":              f.WriterFlags.Flush(),
		"open_output_hook":   f.OutputHookFlags.OpenOutputFileHook(),
		"close_output_hook":  f.OutputHookFlags.CloseOutputFileHook(),
	}
	return o, nil
}

func NewCmdRedupe() *cobra.Command {
	f := NewRedupeFlags()
	cmd := &cobra.Command{
		Use:   "redupe SOURCE...",
		Short: "Reorganize WARC records by year, month or day",
		Long: `Group records by their WARC-Date, using UTC and --interval (year, month or day).
Resolve revisits and deduplicate within each period so each collection can be
replayed independently. Use --audit to check the source collection before rewriting.`,
		ValidArgsFunction: flag.SuffixCompletionFn,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			o, err := f.ToRedupeOptions()
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
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			cmd.SilenceUsage = true
			return o.Run(ctx)
		},
	}
	f.AddFlags(cmd)
	return cmd
}

func (o *RedupeOptions) Complete(cmd *cobra.Command, args []string) error {
	o.reportWriter = cmd.OutOrStdout()
	o.Paths = append(o.Paths, args...)
	return nil
}

func (o *RedupeOptions) checkReportPaths(ctx context.Context) error {
	if o.OutputDir != "" {
		root, err := resolvedPath(o.OutputDir)
		if err != nil {
			return err
		}
		paths := []string{o.IndexDir}
		if o.ReportPath != "" {
			paths = append(paths, o.ReportPath, o.ReportPath+".findings.jsonl")
		}
		for _, p := range paths {
			if p == "" {
				continue
			}
			resolved, err := resolvedPath(p)
			if err != nil {
				return err
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				return err
			}
			parent, err := resolvedPath(filepath.Dir(abs))
			if err != nil {
				return err
			}
			for _, dest := range []string{resolved, filepath.Join(parent, filepath.Base(abs))} {
				if !strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(dest)) {
					continue
				}
				rel, err := filepath.Rel(root, dest)
				if err != nil {
					return err
				}
				first, _, _ := strings.Cut(rel, string(filepath.Separator))
				if managedPeriod(first) {
					return fmt.Errorf("destination inside managed period: %s", p)
				}
			}
		}
	}
	if o.ReportPath != "" {
		return o.FileWalker.CheckOutputPaths(ctx, o.Paths, o.ReportPath, o.ReportPath+".findings.jsonl")
	}
	if o.IndexDir != "" {
		return o.FileWalker.CheckOutputPaths(ctx, o.Paths, filepath.Join(o.IndexDir, "findings.jsonl"))
	}
	return nil
}

func (o *RedupeOptions) Validate() error {
	if err := o.validateInterval(); err != nil {
		return err
	}
	if len(o.Paths) == 0 {
		return errors.New("missing source files or directories")
	}
	if o.FileWalker == nil || o.FileWalker.Fs == nil {
		return errors.New("missing input filesystem")
	}
	if !o.Audit && o.FileWalker.Fs.Name() == "OsFs" {
		output, err := resolvedPath(o.OutputDir)
		if err != nil {
			return err
		}
		for _, path := range o.Paths {
			info, err := os.Stat(path)
			if err != nil || !info.IsDir() {
				continue
			}
			source, err := resolvedPath(path)
			if err != nil {
				return err
			}
			if !strings.EqualFold(filepath.VolumeName(source), filepath.VolumeName(output)) {
				continue
			}
			rel, err := filepath.Rel(source, output)
			if err != nil {
				return err
			}
			if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				return errors.New("output directory must be outside source directories")
			}
		}
	}

	if o.Concurrency < 1 {
		return errors.New("concurrency must be positive")
	}
	if o.MinimumSizeGain < 0 {
		return errors.New("min-size-gain must be nonnegative")
	}
	if !o.Audit && o.WriterConfig == nil {
		return errors.New("missing writer configuration")
	}
	return nil
}

func (o *RedupeOptions) Close() error {
	var err error
	if o.catalog != nil {
		err = errors.Join(err, o.catalog.Close())
		o.catalog = nil
	}
	if o.findings != nil {
		err = errors.Join(err, o.findings.Sync(), o.findings.Close())
		o.findings = nil
	}
	if o.scratch != nil {
		err = errors.Join(err, o.scratch.Close())
		o.scratch = nil
	}
	if o.workDir != "" && !o.keepInventory {
		if e := os.RemoveAll(o.workDir); e != nil {
			err = errors.Join(err, e)
		} else {
			err = errors.Join(err, o.clearInventory())
		}
		o.workDir = ""
	}
	if o.state != nil {
		err = errors.Join(err, o.state.Close())
		o.state = nil
	}
	if o.WriterConfig != nil {
		err = errors.Join(err, o.WriterConfig.Close())
		o.WriterConfig = nil
	}
	if o.FileWalker != nil {
		err = errors.Join(err, o.FileWalker.Close())
		o.FileWalker = nil
	}
	if o.temporaryIndex {
		err = errors.Join(err, os.RemoveAll(o.IndexDir))
		o.temporaryIndex = false
	}
	return err
}

func (o *RedupeOptions) clearInventory() error {
	return o.state.Update(func(tx *badger.Txn) error { return tx.Delete([]byte("!inventory")) })
}

func (o *RedupeOptions) open(ctx context.Context) (err error) {
	if o.IndexDir == "" {
		o.IndexDir, err = os.MkdirTemp("", "warc-redupe-*")
		o.temporaryIndex = err == nil
	} else {
		err = os.MkdirAll(o.IndexDir, 0755)
	}
	if err != nil {
		return err
	}
	// Keep a lock for the whole run, including inventory and publication.
	o.state, err = badger.Open(badger.DefaultOptions(filepath.Join(o.IndexDir, "run-state")).WithLogger(nil).WithSyncWrites(true))
	if err != nil {
		return err
	}
	if err = o.state.Update(func(tx *badger.Txn) error {
		item, e := tx.Get([]byte("!format"))
		if e == nil {
			return item.Value(func(b []byte) error {
				if string(b) != "redupe-ledger-v3" {
					return errors.New("incompatible redupe ledger; run a fresh preview in a new index directory")
				}
				return nil
			})
		}
		if e != badger.ErrKeyNotFound {
			return e
		}
		it := tx.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		it.Rewind()
		if it.Valid() {
			return errors.New("legacy redupe ledger; run a fresh preview in a new index directory")
		}
		return tx.Set([]byte("!format"), []byte("redupe-ledger-v3"))
	}); err != nil {
		return err
	}
	parent, err := filepath.Abs(o.IndexDir)
	if err != nil {
		return err
	}
	if o.Audit && !o.temporaryIndex {
		// Invalidate the previous generation before touching its files.
		if err = o.put("!preview", previewState{Kind: "incomplete"}); err != nil {
			return err
		}
	} else if err = o.get("!preview", &o.preview); err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
		return err
	}

	var stale string
	err = o.get("!inventory", &stale)
	if err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
		return err
	}
	if stale != "" && (filepath.Clean(stale) != stale || filepath.Dir(stale) != parent || !strings.HasPrefix(filepath.Base(stale), "inventory-")) {
		return errors.New("invalid recorded inventory directory")
	}
	if o.preview.Kind != "" {
		if o.preview.Kind != "inventory" || stale == "" {
			return errors.New("incomplete or incompatible preview; run a fresh preview")
		}
		o.workDir, o.keepInventory = stale, true
		for _, file := range []string{"ordered", "scratch/MANIFEST"} {
			if _, err := os.Stat(filepath.Join(stale, file)); err != nil {
				return fmt.Errorf("run a fresh preview: %w", err)
			}
		}
	} else {
		if stale != "" {
			if err = os.RemoveAll(stale); err != nil {
				return err
			}
		}
		next := filepath.Join(parent, "inventory-"+rand.Text())
		if err = o.put("!inventory", next); err != nil {
			return err
		}
		if err = os.Mkdir(next, 0700); err != nil {
			return errors.Join(err, o.clearInventory())
		}
		o.workDir = next
	}
	o.scratch, err = badger.Open(badger.DefaultOptions(filepath.Join(o.workDir, "scratch")).WithLogger(nil).WithSyncWrites(false))
	if err != nil {
		return err
	}
	o.catalog = &inventoryCatalog{o: o}
	auditPath := o.ReportPath + ".findings.jsonl"
	if o.ReportPath == "" {
		auditPath = filepath.Join(o.IndexDir, "findings.jsonl")
	}
	if o.temporaryIndex && o.ReportPath == "" {
		o.findings, err = os.CreateTemp("", "warc-redupe-findings-*.jsonl")
		if err == nil {
			auditPath = o.findings.Name()
		}
	} else {
		o.findings, err = createFindings(auditPath)
	}
	if err == nil {
		o.report.FindingsArtifact = auditPath
	}
	return err
}

func (o *RedupeOptions) validateInterval() error {
	if o.Interval == "" {
		o.Interval = "year"
	}
	switch o.Interval {
	case "year", "month", "day":
		return nil
	default:
		return errors.New("interval must be year, month or day")
	}
}

func managedPeriod(name string) bool {
	if strings.HasPrefix(name, ".redupe-") {
		return true
	}
	for _, layout := range []string{"2006", "2006-01", "2006-01-02"} {
		if _, err := time.Parse(layout, name); err == nil {
			return true
		}
	}
	return false
}

// Resolve existing ancestors of destinations that will be created by open.
func resolvedPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if !errors.Is(err, os.ErrNotExist) {
		return resolved, err
	}
	if _, e := os.Lstat(path); e == nil {
		return "", err
	}
	parent, err := resolvedPath(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

// Replace the directory entry so a hard-linked old findings file is never truncated.
func createFindings(path string) (*os.File, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".findings-*")
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err == nil {
		err = publication.Rename(f.Name(), path, true)
	}
	if err != nil {
		return nil, errors.Join(err, os.Remove(f.Name()))
	}
	return os.OpenFile(path, os.O_WRONLY, 0600)
}
