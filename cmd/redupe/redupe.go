// Package redupe rebuilds yearly WARC collections without discarding captures.
package redupe

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dgraph-io/badger/v4"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/util"
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
	cmd.Flags().Bool("audit", false, "plan without writing replacement WARCs")
	cmd.Flags().Bool("verify-payloads", false, "verify payload digests during audit (always enabled for rewriting)")
	cmd.Flags().String("report", "", "write the audit or transformation report as JSON")
	cmd.Flags().String("index-dir", "", "local working directory; retained run state enables safe restart")
	cmd.Flags().String("min-size-gain", "2KB", "minimum payload size saving required to emit a revisit")
}

type RedupeOptions struct {
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
	catalog                         *cdxstore.DB
	state                           *badger.DB
	workDir                         string
	temporaryIndex                  bool
	report                          Report
}

func (f *RedupeFlags) ToRedupeOptions() (_ *RedupeOptions, err error) {
	o := &RedupeOptions{Audit: viper.GetBool("audit"), VerifyPayloads: viper.GetBool("verify-payloads"),
		ReportPath: viper.GetString("report"), IndexDir: viper.GetString("index-dir"), OutputDir: f.WriterFlags.OutputDir(),
		Concurrency: f.ConcurrencyFlags.Concurrency(), MinimumSizeGain: util.ParseSizeInBytes(viper.GetString("min-size-gain"))}
	defer func() {
		if err != nil {
			err = errors.Join(err, o.Close())
		}
	}()
	if f.WriterFlags.OneToOne() || f.WriterFlags.NameGenerator() == "identity" {
		return nil, errors.New("redupe cannot map one input file to one output file")
	}
	if f.WriterFlags.NameGenerator() != "default" || f.WriterFlags.SubdirPattern() != "" {
		return nil, errors.New("redupe uses yearly directories and the default filename generator")
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
	o.configuration = map[string]any{"policy": "utc-year-preserve-all-v1", "min_size_gain": o.MinimumSizeGain,
		"compress": f.WriterFlags.Compress(), "compression_level": f.WriterFlags.CompressionLevel(),
		"file_size": f.WriterFlags.FileSize(), "prefix": f.WriterFlags.FilePrefix(), "warc_version": f.WriterFlags.WarcVersion(),
		"concurrent_writers": f.WriterFlags.ConcurrentWriters(), "flush": f.WriterFlags.Flush(),
		"open_output_hook": f.OutputHookFlags.OpenOutputFileHook(), "close_output_hook": f.OutputHookFlags.CloseOutputFileHook()}
	return o, nil
}

func NewCmdRedupe() *cobra.Command {
	f := NewRedupeFlags()
	cmd := &cobra.Command{Use: "redupe SOURCE...", Short: "Rebuild self-contained yearly WARC collections",
		Long:              "Rebuild self-contained UTC yearly WARC collections. Every input record is retained\n(or accounted for as an identical alias). There are no record or date filters.\nUnsupported or unresolved payload dependencies block publication of affected years.",
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
		}}
	f.AddFlags(cmd)
	return cmd
}

func (o *RedupeOptions) Complete(cmd *cobra.Command, args []string) error {
	o.reportWriter = cmd.OutOrStdout()
	o.Paths = append(o.Paths, args...)
	return nil
}

func (o *RedupeOptions) Validate() error {
	if strings.HasSuffix(o.ReportPath, ".warc") || strings.HasSuffix(o.ReportPath, ".warc.gz") {
		return errors.New("report must not overwrite a WARC source")
	}
	if !o.Audit {
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
			rel, err := filepath.Rel(source, output)
			if err != nil {
				return err
			}
			if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				return errors.New("output directory must be outside source directories")
			}
		}
	}

	if len(o.Paths) == 0 {
		return errors.New("missing source files or directories")
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
	if o.workDir != "" {
		err = errors.Join(err, os.RemoveAll(o.workDir))
		o.workDir = ""
	}
	if o.temporaryIndex {
		err = errors.Join(err, os.RemoveAll(o.IndexDir))
		o.temporaryIndex = false
	}
	return err
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
	o.workDir, err = os.MkdirTemp(o.IndexDir, "inventory-*")
	if err != nil {
		return err
	}
	o.catalog, err = cdxstore.Open(ctx, cdxstore.Options{Format: "badger", Dir: filepath.Join(o.workDir, "source-catalog")})
	return err
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
