// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"strings"
	"time"

	indexcmd "github.com/nationallibraryofnorway/warchaeology/v5/cmd/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/captures"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type ServeFlags struct{ IndexFlags indexcmd.IndexFlags }

func NewServeFlags() ServeFlags { return ServeFlags{IndexFlags: indexcmd.NewIndexFlags()} }

func (f ServeFlags) AddFlags(cmd *cobra.Command) {
	f.IndexFlags.AddFlags(cmd)
	cmd.Flags().Lookup(flag.IndexFile).Hidden = true
	cmd.Flags().Lookup("wacz-index").Usage = "WACZ indexing: auto imports embedded indexes; warc scans contained WARCs; direct serves one embedded index read-only"
	_ = cmd.Flags().Set("index-format", badgerFormat)
	cmd.Flags().Lookup("index-format").DefValue = badgerFormat
	cmd.Flags().Lookup("index-format").Usage = "badger or zipnum"
	cmd.Flags().Lookup("index-format").Changed = false
	flags := cmd.Flags()
	flags.String("collections-dir", "", "directory of independently writable collections")
	flags.Int64("upload-max-bytes", 128<<20, "maximum decoded CDXJ request bytes")
	flags.Int("upload-max-line", 1<<20, "maximum CDXJ line bytes")
	flags.Duration("upload-timeout", 5*time.Minute, "maximum upload duration")
	flags.Int("query-scan-limit", 10000, "maximum captures examined by interactive queries")
	flags.Int("query-result-limit", 10000, "maximum returned interactive captures")
	flags.String("address", ":9999", "HTTP listen address")
	flags.String("path-prefix", "", "prefix for all routes")
	flags.Bool("read-only", false, "reject ingestion and report mutations")
	flags.Bool("allow-same-origin-replay", false, "enable raw archived HTTP responses on the management origin")
	flags.String("zipnum-dir", "", "immutable published zipnum cluster")
	flags.Int("warcserver-prefix-max-records", 1000, "warcserver prefix result limit")
}

func (f ServeFlags) ToOptions() (*ServeOptions, error) {
	o := &ServeOptions{
		address:        viper.GetString("address"),
		fulltext:       viper.GetBool("fulltext"),
		textDir:        viper.GetString("fulltext-dir"),
		textRebuild:    viper.GetBool("fulltext-rebuild"),
		collectionsDir: viper.GetString("collections-dir"),
		httpOptions: captures.Options{
			MaxBytes:      viper.GetInt64("upload-max-bytes"),
			MaxLine:       viper.GetInt("upload-max-line"),
			UploadTimeout: viper.GetDuration("upload-timeout"),
			ScanLimit:     viper.GetInt("query-scan-limit"),
			ResultLimit:   viper.GetInt("query-result-limit"),
		},
		prefix:                strings.TrimRight(viper.GetString("path-prefix"), "/"),
		format:                viper.GetString("index-format"),
		zipnumDir:             viper.GetString("zipnum-dir"),
		readOnly:              viper.GetBool("read-only"),
		allowSameOriginReplay: viper.GetBool("allow-same-origin-replay"),
		hasInputs:             viper.GetString("source-file-list") != "",
		prefixLimit:           viper.GetInt("warcserver-prefix-max-records"),
		waczMode:              viper.GetString("wacz-index"),
	}
	if o.waczMode == directWacz || o.collectionsDir != "" || o.format == zipnumFormat {
		return o, nil
	}
	var err error
	o.database, err = f.IndexFlags.CaptureDatabaseFlags.ToOptions()
	if err != nil {
		return nil, err
	}
	o.database.ReadOnly = o.readOnly
	if !o.readOnly {
		o.ingestion, err = f.IndexFlags.ToOptions()
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}
