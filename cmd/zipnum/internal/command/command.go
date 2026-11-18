// Package command shares flag and option lifecycles for zipnum operations.
package command

import (
	"os/signal"
	"runtime"
	"syscall"

	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipnum"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type ZipnumFlags struct{}

func (ZipnumFlags) AddFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("output-dir", "cluster", "new cluster directory")
	f.Bool("verify-sources", false, "verify source parts and boundary blocks")
	f.Int("concurrency", max(runtime.NumCPU()/2, 1), "partition workers")
	f.Int("lines", 0, "lines per block (inherit when applicable)")
	f.Int("blocks", 0, "blocks per part (inherit when applicable)")
}

func (ZipnumFlags) ToOptions() *ZipnumOptions {
	return &ZipnumOptions{
		Output:        viper.GetString("output-dir"),
		VerifySources: viper.GetBool("verify-sources"),
		Concurrency:   viper.GetInt("concurrency"),
		LinesPerBlock: viper.GetInt("lines"),
		BlocksPerPart: viper.GetInt("blocks")}
}

type ZipnumOptions struct{ zipnum.Options }

func (o *ZipnumOptions) Complete(cmd *cobra.Command, args []string) error {
	o.Inputs = args
	o.LinesSet = flag.Explicit(cmd, "lines")
	o.BlocksSet = flag.Explicit(cmd, "blocks")
	switch cmd.Name() {
	case "add":
		o.Inputs, o.Add = args[:1], args[1]
	case "merge":
		o.Merge = true
	case "repartition":
		o.Repartition = true
	}
	return nil
}

func NewOperationCommand(use, short string, args cobra.PositionalArgs) *cobra.Command {
	flags := ZipnumFlags{}
	cmd := &cobra.Command{
		Use: use, Short: short, Args: args,
		RunE: func(cmd *cobra.Command, args []string) error {
			o := flags.ToOptions()
			if err := o.Complete(cmd, args); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
				return err
			}
			cmd.SilenceUsage = true
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			return o.Run(ctx)
		},
	}
	flags.AddFlags(cmd)
	return cmd
}
