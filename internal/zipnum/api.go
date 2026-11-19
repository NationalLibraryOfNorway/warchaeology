package zipnum

import (
	"context"
)

// Options configures an immutable cluster operation. A completed output is
// published only after its indexes and metadata have been validated.
type Options struct {
	Inputs                                    []string
	Output, Add                               string
	Merge, Repartition, VerifySources         bool
	Concurrency, LinesPerBlock, BlocksPerPart int
	LinesSet, BlocksSet                       bool
}

func (o Options) Run(ctx context.Context) error {
	return o.options().run(ctx)
}

func (o Options) Validate() error { return o.options().validate() }

func (o Options) options() options {
	return options{
		inputs:        o.Inputs,
		output:        o.Output,
		add:           o.Add,
		merge:         o.Merge,
		repartition:   o.Repartition,
		verifySources: o.VerifySources,
		concurrency:   o.Concurrency,
		linesPerBlock: o.LinesPerBlock,
		blocksPerPart: o.BlocksPerPart,
		linesSet:      o.LinesSet,
		blocksSet:     o.BlocksSet,
	}
}
