package add

import (
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/zipnum/internal/command"
	"github.com/spf13/cobra"
)

func NewCmdAdd() *cobra.Command {
	return command.NewOperationCommand("add CLUSTER CDXJ", "Add a sorted CDXJ file to a new cluster", cobra.ExactArgs(2))
}
