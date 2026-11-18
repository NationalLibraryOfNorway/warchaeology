package build

import (
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/zipnum/internal/command"
	"github.com/spf13/cobra"
)

func NewCmdBuild() *cobra.Command {
	return command.NewOperationCommand("build INPUT...", "Build a cluster from sorted CDXJ files", cobra.MinimumNArgs(1))
}
