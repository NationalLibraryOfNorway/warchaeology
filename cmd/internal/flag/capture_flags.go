package flag

import (
	"path/filepath"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type CaptureDatabaseFlags struct{}

func (CaptureDatabaseFlags) AddFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("badger-dir", "./warcdb", "Badger database directory")
	f.String("badger-database", "", "Badger database name")
}

func (CaptureDatabaseFlags) ToOptions() cdxstore.Options {
	format := viper.GetString("index-format")
	dir := filepath.Join(viper.GetString("badger-dir"), viper.GetString("badger-database"))

	return cdxstore.Options{Format: format, Dir: dir}
}
