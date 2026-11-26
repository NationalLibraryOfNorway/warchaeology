// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package flag

import (
	"os"

	"github.com/nationallibraryofnorway/gowarc/v4"
	"github.com/nationallibraryofnorway/whatwg-url/url"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	TempDir     = "tmp-dir"
	TempDirHelp = `directory used for temporary files`

	StrictValidation     = "strict"
	StrictValidationHelp = `fail on the first validation error`

	LenientValidation     = "lenient"
	LenientValidationHelp = `minimize validation for faster, more permissive parsing`

	LaxHostParsing     = "lax-host-parsing"
	LaxHostParsingHelp = `allow lenient host parsing in URL parsing`
)

type WarcRecordOptionFlags struct {
}

func (f WarcRecordOptionFlags) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.String(TempDir, os.TempDir(), TempDirHelp)
	flags.Bool(StrictValidation, false, StrictValidationHelp)
	flags.Bool(LenientValidation, false, LenientValidationHelp)
	flags.Bool(LaxHostParsing, false, LaxHostParsingHelp)
}

func (f WarcRecordOptionFlags) TempDir() string {
	return viper.GetString(TempDir)
}

func (f WarcRecordOptionFlags) StrictValidation() bool {
	return viper.GetBool(StrictValidation)
}

func (f WarcRecordOptionFlags) LenientValidation() bool {
	return viper.GetBool(LenientValidation)
}

func (f WarcRecordOptionFlags) LaxHostParsing() bool {
	return viper.GetBool(LaxHostParsing)
}

func (f WarcRecordOptionFlags) ToWarcRecordOptions() []gowarc.WarcRecordOption {
	return f.recordOptions(false)
}

// IndexRecordOptions keeps protocol blocks available for capture metadata extraction.
func (f WarcRecordOptionFlags) IndexRecordOptions() []gowarc.WarcRecordOption {
	return f.recordOptions(true)
}

func (f WarcRecordOptionFlags) recordOptions(parseBlocks bool) []gowarc.WarcRecordOption {
	options := []gowarc.WarcRecordOption{
		gowarc.WithBufferTmpDir(f.TempDir()),
	}
	if f.LenientValidation() {
		if parseBlocks {
			// gowarc caches the block during spec validation. Keep it readable after Next returns.
			options = append(options, gowarc.WithSyntaxErrorPolicy(gowarc.ErrIgnore), gowarc.WithSpecViolationPolicy(gowarc.ErrWarn), gowarc.WithUnknownRecordTypePolicy(gowarc.ErrIgnore))
		} else {
			options = append(options, gowarc.WithNoValidation())
		}
	}
	if f.StrictValidation() {
		options = append(options, gowarc.WithStrictValidation())
	}
	if f.LaxHostParsing() {
		options = append(options, gowarc.WithUrlParserOptions(url.WithLaxHostParsing()))
	}
	return options
}
