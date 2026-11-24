// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package flag

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type CaptureDatabaseFlags struct{}

const IndexFile = "index-file"

func (CaptureDatabaseFlags) AddFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String(IndexDir, "", "Badger index database directory (default: warchaeology/warcdb in the user data directory)")
	f.String("badger-database", "", "Badger database name")
}

func defaultCaptureDirectory() (string, error) {
	var base string
	if runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
	} else if runtime.GOOS != "darwin" {
		base = os.Getenv("XDG_DATA_HOME")
		if !filepath.IsAbs(base) {
			base = ""
		}
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		switch runtime.GOOS {
		case "windows":
			base = filepath.Join(home, "AppData", "Local")
		case "darwin":
			base = filepath.Join(home, "Library", "Application Support")
		default:
			base = filepath.Join(home, ".local", "share")
		}
	}
	return filepath.Join(base, "warchaeology", "warcdb"), nil
}

func (CaptureDatabaseFlags) ToOptions() (cdxstore.Options, error) {
	format := viper.GetString("index-format")
	dir := viper.GetString(IndexDir)
	if dir == "" {
		var err error
		dir, err = defaultCaptureDirectory()
		if err != nil {
			return cdxstore.Options{}, fmt.Errorf("set --index-dir to select a capture database: %w", err)
		}
	}
	dir = filepath.Join(dir, viper.GetString("badger-database"))

	return cdxstore.Options{Format: format, Dir: dir}, nil
}
