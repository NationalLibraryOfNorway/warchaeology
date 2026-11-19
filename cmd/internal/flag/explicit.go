// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package flag

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Explicit distinguishes supplied configuration from a registered flag default.
func Explicit(cmd *cobra.Command, name string) bool {
	_, env := os.LookupEnv("WARC_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")))
	return cmd.Flags().Changed(name) || viper.InConfig(name) || env
}
