// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package util

// CheckFileDescriptorLimit does nothing when OS is not Linux
func CheckFileDescriptorLimit(limit uint64) error {
	return nil
}
