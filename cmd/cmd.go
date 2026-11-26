// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/cat"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/console"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/convert"
	convertwarc "github.com/nationallibraryofnorway/warchaeology/v5/cmd/convert/warc"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/dedup"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/ls"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/redupe"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/serve"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/validate"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/version"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/zipnum"
	archivefs "github.com/nationallibraryofnorway/warchaeology/v5/internal/fs"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// NewWarcCommand returns a new cobra.Command implementing the root command for warc
func NewWarcCommand() *cobra.Command {
	var logCloser io.Closer

	cmd := &cobra.Command{
		Use:   "warc",
		Short: "A tool for handling warc files",
		Long:  ``,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			err := loadConfig(cmd.Flags())
			if err != nil {
				return err
			}
			endpoints := viper.GetStringSlice("ftp-endpoints")
			if input := viper.GetString("input-file"); strings.HasPrefix(strings.ToLower(input), "ftp://") {
				endpoints = append(endpoints, input)
			}
			ctx, err := archivefs.WithFTPEndpoints(cmd.Context(), endpoints)
			if err != nil {
				return err
			}
			cmd.SetContext(ctx)
			logCloser, err = initLogger(os.Stderr, flag.LogFileName(), flag.LogFormat(), flag.LogLevel())
			return err
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			if logCloser != nil {
				_ = logCloser.Close()
			}
		},
	}

	// Add flags
	flag.AddPersistentFlags(cmd)

	cmd.AddCommand(index.NewCmdIndex(), serve.NewCmdServe(), zipnum.NewCmdZipnum())

	// Add subcommands
	cmd.AddCommand(ls.NewCmdList())           // ls
	cmd.AddCommand(cat.NewCmdCat())           // cat
	cmd.AddCommand(validate.NewCmdValidate()) // validate
	cmd.AddCommand(console.NewCmdConsole())   // console
	cmd.AddCommand(convert.NewCmdConvert())   // convert
	cmd.AddCommand(redupe.NewCmdRedupe(), redupe.NewCmdAudit(), redupe.NewCmdRecover(), convertwarc.NewCmdRepair())
	cmd.AddCommand(dedup.NewCmdDedup())     // dedup
	cmd.AddCommand(version.NewCmdVersion()) // version

	return cmd
}

func loadConfig(flags *pflag.FlagSet) error {
	viper.SetEnvPrefix("WARC")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()
	err := viper.BindPFlags(flags)
	if err != nil {
		return err
	}

	if viper.IsSet("config") {
		// Read config file specified by 'config' flag
		viper.SetConfigFile(viper.GetString("config"))
		return viper.ReadInConfig()
	}

	// Read config file from default locations
	viper.SetConfigName("config") // name of config file (without extension)
	viper.SetConfigType("yaml")   // REQUIRED if the config file does not have the extension in the name

	// current directory
	workingDirectory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %v", err)
	}
	viper.AddConfigPath(workingDirectory)
	if userConfigDir, err := os.UserConfigDir(); err == nil {
		viper.AddConfigPath(filepath.Join(userConfigDir, "warc"))
	}
	for _, root := range systemConfigRoots() {
		viper.AddConfigPath(filepath.Join(root, "warc"))
	}

	return readConfigFile()
}

func systemConfigRoots() []string {
	switch runtime.GOOS {
	case "windows":
		if root := os.Getenv("PROGRAMDATA"); root != "" {
			return []string{root}
		}
		return nil
	case "darwin", "ios":
		return []string{"/Library/Application Support"}
	default:
		raw := os.Getenv("XDG_CONFIG_DIRS")
		if raw == "" {
			return []string{"/etc/xdg"}
		}
		var roots []string
		for _, root := range filepath.SplitList(raw) {
			if filepath.IsAbs(root) {
				roots = append(roots, root)
			}
		}
		return roots
	}
}

func readConfigFile() error {
	err := viper.ReadInConfig()
	if err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Config file not found; ignore error
			return nil
		}
		return err
	}
	return nil
}

func initLogger(out io.WriteCloser, file string, format string, level string) (io.Closer, error) {
	var err error
	var w io.WriteCloser

	if file == "-" {
		w = out
	} else {
		w, err = os.Create(file)
		if err != nil {
			return nil, err
		}
	}

	levelVar := new(slog.LevelVar)
	levelVar.Set(toLogLevel(level))

	opts := &slog.HandlerOptions{Level: levelVar}

	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}

	slog.SetDefault(slog.New(handler))

	return w, nil
}

func toLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
