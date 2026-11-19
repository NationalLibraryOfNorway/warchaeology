// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package model

import "github.com/awesome-gocui/gocui"

type KeyBinding struct {
	Key any
	Mod gocui.Modifier
	Fn  func(*gocui.Gui, *gocui.View) error
}
