// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"fmt"
	"uuid"
)

const (
	defaultLines  = 3000
	defaultBlocks = 32
)

type options struct {
	inputs        []string
	input         string
	output        string
	add           string
	verifySources bool
	repartition   bool
	merge         bool
	concurrency   int
	linesPerBlock int
	blocksPerPart int
	linesSet      bool
	blocksSet     bool
	newPartID     func() uuid.UUID
}

// validate checks flag combinations before any input or output is opened.
func (o options) validate() error {
	modes := 0
	if o.repartition {
		modes++
	}
	if o.merge {
		modes++
	}
	if o.add != "" {
		modes++
	}
	if modes > 1 {
		return fmt.Errorf("-add, -merge, and -repartition are mutually exclusive")
	}
	if o.concurrency <= 0 {
		return fmt.Errorf("concurrency must be positive")
	}
	if (o.linesSet && o.linesPerBlock <= 0) || (o.blocksSet && o.blocksPerPart <= 0) {
		return fmt.Errorf("line and block counts must be positive")
	}
	if o.output == "" {
		return fmt.Errorf("output path must not be empty")
	}
	return nil
}

// resolveLayout applies mode defaults before explicit line and block overrides.
// Add inherits its source layout; merge inherits only when both sources agree.
func (o options) resolveLayout(clusters []clusterManifest) (int, int) {
	lines, blocks := defaultLines, defaultBlocks
	if o.add != "" && len(clusters) == 1 {
		lines, blocks = clusters[0].meta.LinesPerBlock, clusters[0].meta.BlocksPerPart
	} else if o.merge && len(clusters) == 2 &&
		clusters[0].meta.LinesPerBlock == clusters[1].meta.LinesPerBlock &&
		clusters[0].meta.BlocksPerPart == clusters[1].meta.BlocksPerPart {
		lines, blocks = clusters[0].meta.LinesPerBlock, clusters[0].meta.BlocksPerPart
	}
	if o.linesSet || o.linesPerBlock != 0 {
		lines = o.linesPerBlock
	}
	if o.blocksSet || o.blocksPerPart != 0 {
		blocks = o.blocksPerPart
	}
	return lines, blocks
}
