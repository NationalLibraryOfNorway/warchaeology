package zipnum

import (
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/zipnum/add"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/zipnum/build"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/zipnum/merge"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/zipnum/repartition"
	"github.com/spf13/cobra"
)

func NewCmdZipnum() *cobra.Command {
	cmd := &cobra.Command{Use: "zipnum", Short: "Build and transform immutable zipnum clusters"}
	cmd.AddCommand(build.NewCmdBuild(), add.NewCmdAdd(), merge.NewCmdMerge(), repartition.NewCmdRepartition())
	return cmd
}
