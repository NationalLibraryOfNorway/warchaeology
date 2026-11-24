// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package flag

import (
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/bytesize"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	MinDiskFree     = "min-disk-free"
	MinDiskFreeHelp = `minimum free disk space required to continue writing WARC output`
)

type UtilFlags struct {
}

func (u UtilFlags) AddFlags(cmd *cobra.Command) {
	cmd.Flags().String(MinDiskFree, "1GB", MinDiskFreeHelp)
}

func (u UtilFlags) MinFreeDisk() int64 {
	minFreeDisk := viper.GetString(MinDiskFree)
	return bytesize.Parse(minFreeDisk)
}
