package serve

import (
	"strings"

	indexcmd "github.com/nationallibraryofnorway/warchaeology/v5/cmd/index"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type ServeFlags struct{ IndexFlags indexcmd.IndexFlags }

func NewServeFlags() ServeFlags { return ServeFlags{IndexFlags: indexcmd.NewIndexFlags()} }

func (f ServeFlags) AddFlags(cmd *cobra.Command) {
	f.IndexFlags.AddFlags(cmd)
	_ = cmd.Flags().Set("index-format", "badger")
	cmd.Flags().Lookup("index-format").DefValue = "badger"
	cmd.Flags().Lookup("index-format").Usage = "badger or zipnum"
	cmd.Flags().Lookup("index-format").Changed = false
	flags := cmd.Flags()
	flags.String("address", ":9999", "HTTP listen address")
	flags.String("path-prefix", "", "prefix for all routes")
	flags.Bool("read-only", false, "reject ingestion and report mutations")
	flags.String("zipnum-dir", "", "immutable published zipnum cluster")
	flags.Int("warcserver-prefix-max-records", 1000, "warcserver prefix result limit")
}

func (f ServeFlags) ToOptions() (*ServeOptions, error) {
	o := &ServeOptions{
		address:     viper.GetString("address"),
		prefix:      strings.TrimRight(viper.GetString("path-prefix"), "/"),
		format:      viper.GetString("index-format"),
		zipnumDir:   viper.GetString("zipnum-dir"),
		readOnly:    viper.GetBool("read-only"),
		hasInputs:   viper.GetString("source-file-list") != "",
		prefixLimit: viper.GetInt("warcserver-prefix-max-records"),
		database:    f.IndexFlags.CaptureDatabaseFlags.ToOptions(),
	}
	o.database.ReadOnly = o.readOnly
	if o.format != zipnumFormat && !o.readOnly {
		var err error
		o.ingestion, err = f.IndexFlags.ToOptions()
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}
