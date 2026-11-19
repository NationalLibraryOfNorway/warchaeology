// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"errors"
	"os"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/spf13/cobra"
)

func addMaintenanceCommands(cmd *cobra.Command) {
	cmd.AddCommand(&cobra.Command{
		Use:   "restore BACKUP NEW-DIRECTORY",
		Short: "Restore an index database backup offline into a new directory",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return restoreIndex(cmd.Context(), args[0], args[1])
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "compact DIRECTORY",
		Short: "Reclaim index database storage offline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if _, e := os.Stat(args[0]); e != nil {
				return e
			}
			db, e := cdxstore.Open(cmd.Context(), cdxstore.Options{Format: "badger", Dir: args[0]})
			if e != nil {
				return e
			}
			defer func() { err = errors.Join(err, db.Close()) }()
			return db.Compact(cmd.Context())
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "backup DIRECTORY BACKUP",
		Short: "Back up an index database offline",
		Long:  "Back up a stopped index database and its fulltext subdirectory to a new archive. Source WARC and WACZ files are not included.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return backupIndex(cmd.Context(), args[0], args[1])
		},
	})
}
