package reset

import (
	"context"
	"errors"

	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/spf13/cobra"
)

type ResetFlags struct{ CaptureDatabaseFlags flag.CaptureDatabaseFlags }

func (f ResetFlags) AddFlags(cmd *cobra.Command) {
	cmd.Flags().String("index-format", "badger", "database backend: badger")
	f.CaptureDatabaseFlags.AddFlags(cmd)
}

func (f ResetFlags) ToOptions() (*ResetOptions, error) {
	database, err := f.CaptureDatabaseFlags.ToOptions()
	if err != nil {
		return nil, err
	}
	return &ResetOptions{database: database}, nil
}

type ResetOptions struct {
	database cdxstore.Options
	db       *cdxstore.DB
}

func (o *ResetOptions) Run(ctx context.Context) error {
	var err error
	o.db, err = cdxstore.Open(ctx, o.database)
	if err != nil {
		return err
	}
	return o.db.Delete(ctx)
}

func (o *ResetOptions) Close() error {
	if o.db == nil {
		return nil
	}
	err := o.db.Close()
	o.db = nil
	return err
}

func NewCmdReset() *cobra.Command {
	flags := ResetFlags{}
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset the selected index database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			o, err := flags.ToOptions()
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, o.Close()) }()
			cmd.SilenceUsage = true
			return o.Run(cmd.Context())
		},
	}
	flags.AddFlags(cmd)
	return cmd
}
