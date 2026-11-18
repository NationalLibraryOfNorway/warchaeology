package zipnum

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"runtime"
	"syscall"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
	GoVersion = runtime.Version()
	Platform  = runtime.GOOS + "/" + runtime.GOARCH
)

// cliOptions keeps display flags separate from the cluster operation's options.
type cliOptions struct {
	options options
	debug   bool
	version bool
}

// parseCLI decodes flags and remembers whether layout overrides were explicit.
func parseCLI(args []string, stdout io.Writer) (cliOptions, error) {
	flags := flag.NewFlagSet("zipnum", flag.ContinueOnError)
	flags.SetOutput(stdout)
	var cli cliOptions
	o := &cli.options
	flags.StringVar(&o.input, "in", "", "Glob pattern(s) or comma-separated file paths (supports .gz)")
	flags.StringVar(&o.output, "out", "cluster", "Output directory")
	flags.BoolVar(&o.repartition, "repartition", false, "Repartition exactly one zipnum cluster")
	flags.BoolVar(&o.verifySources, "verify-sources", false, "Verify source cluster indexes, byte ranges, and boundary blocks")
	flags.BoolVar(&o.merge, "merge", false, "Merge exactly two zipnum cluster directories")
	flags.StringVar(&o.add, "add", "", "Add one sorted CDXJ index to the cluster given by -in")
	flags.IntVar(&o.concurrency, "concurrency", max(runtime.NumCPU()/2, 1), "Number of concurrent partition workers")
	flags.IntVar(&o.blocksPerPart, "blocks", 0, "Blocks per part (mode-dependent default)")
	flags.IntVar(&o.linesPerBlock, "lines", 0, "Lines per block (mode-dependent default)")
	flags.BoolVar(&cli.debug, "debug", false, "Enable debug logging")
	flags.BoolVar(&cli.version, "version", false, "Print version information and exit")
	if err := flags.Parse(args); err != nil {
		return cliOptions{}, err
	}
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "lines":
			o.linesSet = true
		case "blocks":
			o.blocksSet = true
		}
	})
	return cli, nil
}

// runCLI handles display and logging, then runs the requested cluster operation.
func runCLI(ctx context.Context, args []string, stdout io.Writer) error {
	cli, err := parseCLI(args, stdout)
	if err != nil {
		return err
	}
	if cli.version {
		_, err := fmt.Fprintf(stdout, "Version:    %s\nCommit:     %s\nBuildTime:  %s\nGoVersion:  %s\nPlatform:   %s\n", Version, Commit, BuildTime, GoVersion, Platform)
		return err
	}
	level := slog.LevelInfo
	if cli.debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: level})))
	slog.Info("Zipnum", "version", Version)
	return cli.options.run(ctx)
}

// realMain cancels work on termination signals and maps errors to exit status.
func realMain(args []string, stdout io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := runCLI(ctx, args, stdout); err != nil {
		slog.Error(err.Error())
		return 1
	}
	return 0
}
