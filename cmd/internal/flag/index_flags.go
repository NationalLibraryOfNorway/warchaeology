// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package flag

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	NewIndex     = "new-index"
	NewIndexHelp = `start with a fresh index by deleting any existing index in --index-dir at startup`

	KeepIndex     = "keep-index"
	KeepIndexHelp = `keep index files in --index-dir after the run so later runs can continue from them`

	IndexDir     = "index-dir"
	IndexDirHelp = `directory used to store index data`
)

type IndexFlags struct{}

func (f IndexFlags) AddFlags(cmd *cobra.Command) {
	cacheDir, err := os.UserCacheDir()
	if err == nil {
		cacheDir = filepath.Join(cacheDir, "warchaeology", cmd.Name())
	}

	flags := cmd.Flags()
	flags.BoolP(KeepIndex, "k", false, KeepIndexHelp)
	flags.BoolP(NewIndex, "K", false, NewIndexHelp)
	flags.String(IndexDir, cacheDir, IndexDirHelp)

	if err := cmd.MarkFlagDirname(IndexDir); err != nil {
		panic(err)
	}
}

func (f IndexFlags) IndexDir() string {
	return viper.GetString(IndexDir)
}

func (f IndexFlags) KeepIndex() bool {
	return viper.GetBool(KeepIndex)
}

func (f IndexFlags) NewIndex() bool {
	return viper.GetBool(NewIndex)
}

func (f IndexFlags) ToDigestIndex() (*index.DigestIndex, error) {
	dir := f.IndexDir()
	if dir == "" {
		return nil, errors.New("set --index-dir to select an index directory")
	}
	return index.NewDigestIndex(dir, f.KeepIndex(), f.NewIndex())
}

func (f IndexFlags) ToFileIndex() (*index.FileIndex, error) {
	dir := f.IndexDir()
	if dir == "" {
		return nil, errors.New("set --index-dir to select an index directory")
	}
	return index.NewFileIndex(dir, f.KeepIndex(), f.NewIndex())
}
