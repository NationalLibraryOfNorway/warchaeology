// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package flag

import (
	"github.com/nationallibraryofnorway/gowarc/v4"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	Repair     = "repair"
	RepairHelp = `deprecated: attempt record repair (see warc repair and dedup --add-missing-digests)`
)

type RepairFlags struct {
	Always bool
}

func (r RepairFlags) AddFlags(cmd *cobra.Command) {
	cmd.Flags().BoolP(Repair, "R", false, RepairHelp)
}

func (r RepairFlags) Repair() bool {
	return r.Always || viper.GetBool(Repair)
}

func (r RepairFlags) ToWarcRecordOptions() []gowarc.WarcRecordOption {
	if !r.Repair() {
		return nil
	}
	return []gowarc.WarcRecordOption{
		gowarc.WithSyntaxErrorPolicy(gowarc.ErrWarn),
		gowarc.WithSpecViolationPolicy(gowarc.ErrWarn),
		gowarc.WithAddMissingDigest(true),
		gowarc.WithFixSyntaxErrors(true),
		gowarc.WithFixDigest(true),
		gowarc.WithAddMissingContentLength(true),
		gowarc.WithAddMissingRecordId(true),
		gowarc.WithFixContentLength(true),
		gowarc.WithFixWarcFieldsBlockErrors(true),
	}
}
