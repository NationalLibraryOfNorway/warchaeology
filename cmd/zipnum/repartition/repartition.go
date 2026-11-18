package repartition

import (
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/zipnum/internal/command"
	"github.com/spf13/cobra"
)

func NewCmdRepartition() *cobra.Command {
	return command.NewOperationCommand("repartition CLUSTER", "Repartition a cluster into a new cluster", cobra.ExactArgs(1))
}
