// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package merge

import (
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/zipnum/internal/command"
	"github.com/spf13/cobra"
)

func NewCmdMerge() *cobra.Command {
	return command.NewOperationCommand("merge CLUSTER_A CLUSTER_B", "Merge two clusters into a new cluster", cobra.ExactArgs(2))
}
