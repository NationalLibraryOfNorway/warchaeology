// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package widget

import (
	"errors"
	"fmt"
	"strings"

	"github.com/awesome-gocui/gocui"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/ui/model"
)

type Entry[T any] struct {
	Item    T
	Display string
	Search  string
}

type ListWidget[T any] struct {
	viewName string

	items   []Entry[T]
	visible []int

	filter   string
	selected int
	offset   int

	makeText     func(T) string
	makeColor    func(T) string // optional; returns a pre-escaped fg color sequence
	makeSubtitle func(T) string // optional; extra info appended to "N/M" subtitle
	matches      func(T) bool

	nav Controller

	onSelectionChanged func(item T, ok bool)
}

func NewListWidget[T any](
	viewName string,
	makeText func(T) string,
	nav Controller,
	onSelectionChanged func(item T, ok bool),
) *ListWidget[T] {
	return &ListWidget[T]{
		viewName:           viewName,
		makeText:           makeText,
		nav:                nav,
		onSelectionChanged: onSelectionChanged,
		selected:           -1,
	}
}

func (w *ListWidget[T]) Layout(gui *gocui.Gui, x0, y0, x1, y1 int) error {
	v, err := gui.SetView(w.viewName, x0, y0, x1, y1, 0)
	if err != nil && !errors.Is(err, gocui.ErrUnknownView) {
		return err
	}
	if errors.Is(err, gocui.ErrUnknownView) {
		v.Title = w.viewName
		v.Highlight = false
		v.Autoscroll = false
		keyBindings := []model.KeyBinding{
			{Key: gocui.KeyArrowDown, Mod: gocui.ModNone, Fn: w.onCursorDown},
			{Key: gocui.KeyArrowUp, Mod: gocui.ModNone, Fn: w.onCursorUp},
			{Key: gocui.KeyHome, Mod: gocui.ModNone, Fn: w.onGotoStart},
			{Key: gocui.KeyEnd, Mod: gocui.ModNone, Fn: w.onGotoEnd},
			{Key: gocui.KeyPgdn, Mod: gocui.ModNone, Fn: w.onPageDown},
			{Key: gocui.KeyPgup, Mod: gocui.ModNone, Fn: w.onPageUp},
			{Key: 'f', Mod: gocui.ModNone, Fn: w.onSearch},
			{Key: '/', Mod: gocui.ModNone, Fn: w.onSearch},
			{Key: gocui.MouseLeft, Mod: gocui.ModNone, Fn: w.onCurrentView},
			{Key: gocui.MouseWheelDown, Mod: gocui.ModNone, Fn: w.onCursorDown},
			{Key: gocui.MouseWheelUp, Mod: gocui.ModNone, Fn: w.onCursorUp},
		}
		for _, kb := range keyBindings {
			if err := gui.SetKeybinding(w.viewName, kb.Key, kb.Mod, kb.Fn); err != nil {
				return err
			}
		}
	}
	_, height := v.Size()
	height = max(1, height)
	// Keep the highlight on screen and fill newly available rows after a resize.
	first := max(0, w.selected-height+1)
	last := max(0, min(w.selected, len(w.visible)-height))
	w.offset = max(first, min(w.offset, last))
	w.draw(v, height)
	return nil
}

func (w *ListWidget[T]) VisibleCount() int { return len(w.visible) }

func (w *ListWidget[T]) SetColorizer(f func(T) string)     { w.makeColor = f }
func (w *ListWidget[T]) SetSubtitleMaker(f func(T) string) { w.makeSubtitle = f }

func (w *ListWidget[T]) SetPredicate(f func(T) bool) {
	w.matches = f
	w.refilter()
}

func (w *ListWidget[T]) SetFilter(q string) {
	w.filter = normalize(q)
	w.refilter()
}

func (w *ListWidget[T]) selectedIndex() int {
	if w.selected < 0 {
		return -1
	}
	return w.visible[w.selected]
}

func (w *ListWidget[T]) refilter() {
	previous := w.selectedIndex()
	w.visible = w.visible[:0]
	w.selected = -1
	for i, entry := range w.items {
		if w.include(entry) {
			if i == previous {
				w.selected = len(w.visible)
			}
			w.visible = append(w.visible, i)
		}
	}
	if w.selected < 0 && len(w.visible) > 0 {
		w.selected = 0
	}
	if w.selectedIndex() != previous {
		w.onSelectionChanged(w.SelectedItem())
	}
}

func (w *ListWidget[T]) include(e Entry[T]) bool {
	return strings.Contains(e.Search, w.filter) && (w.matches == nil || w.matches(e.Item))
}

func (w *ListWidget[T]) Clear() {
	w.items, w.visible = nil, nil
	w.selected, w.offset = -1, 0
	w.onSelectionChanged(w.SelectedItem())
}

func (w *ListWidget[T]) AddElements(items []T) {
	for _, item := range items {
		display := w.makeText(item)
		entry := Entry[T]{item, display, normalize(display)}
		if w.include(entry) {
			w.visible = append(w.visible, len(w.items))
		}
		w.items = append(w.items, entry)
	}
	if w.selected < 0 && len(w.visible) > 0 {
		w.setSelected(0)
	}
}

func (w *ListWidget[T]) draw(v *gocui.View, height int) {
	n, selected, offset := len(w.visible), w.selected, w.offset
	v.Clear()

	end := min(offset+height, n)

	for row := offset; row < end; row++ {
		entry := w.items[w.visible[row]]
		if row == selected {
			_, _ = fmt.Fprintf(v, "%s> %s%s%s%s\n", model.SGRReset, model.SelectionBg, model.SelectionFg, entry.Display, model.SGRReset)
		} else if w.makeColor != nil {
			_, _ = fmt.Fprintf(v, "%s%s  %s%s\n", model.SGRReset, w.makeColor(entry.Item), entry.Display, model.ColorReset)
		} else {
			_, _ = fmt.Fprintf(v, "%s  %s\n", model.SGRReset, entry.Display)
		}
	}

	v.Subtitle = fmt.Sprintf("%d/%d", selected+1, n)
	if w.makeSubtitle != nil && selected >= 0 {
		if extra := w.makeSubtitle(w.items[w.visible[selected]].Item); extra != "" {
			v.Subtitle = extra + "  " + v.Subtitle
		}
	}
}

func (w *ListWidget[T]) SelectedItem() (T, bool) {
	if i := w.selectedIndex(); i >= 0 {
		return w.items[i].Item, true
	}
	var zero T
	return zero, false
}

func (w *ListWidget[T]) setSelected(row int) {
	if len(w.visible) == 0 {
		return
	}
	row = max(0, min(row, len(w.visible)-1))
	if row != w.selected {
		w.selected = row
		w.onSelectionChanged(w.SelectedItem())
	}
}

func (w *ListWidget[T]) moveSelection(delta int) error {
	w.setSelected(w.selected + delta)
	return nil
}

func (w *ListWidget[T]) pageSelection(v *gocui.View, dir int) error {
	_, h := v.Size()
	if h < 2 {
		h = 2
	}
	w.setSelected(w.selected + dir*(h-1))
	return nil
}

func (w *ListWidget[T]) onCurrentView(_ *gocui.Gui, v *gocui.View) error {
	w.nav.SetCurView(v.Name())

	_, cy := v.Cursor()

	w.setSelected(w.offset + cy)
	return nil
}

func (w *ListWidget[T]) onGotoStart(_ *gocui.Gui, _ *gocui.View) error {
	w.setSelected(0)
	return nil
}

func (w *ListWidget[T]) onGotoEnd(_ *gocui.Gui, _ *gocui.View) error {
	last := len(w.visible) - 1
	w.setSelected(last)
	return nil
}

func (w *ListWidget[T]) onPageUp(_ *gocui.Gui, v *gocui.View) error {
	return w.pageSelection(v, -1)
}

func (w *ListWidget[T]) onPageDown(_ *gocui.Gui, v *gocui.View) error {
	return w.pageSelection(v, 1)
}

func (w *ListWidget[T]) onCursorDown(_ *gocui.Gui, _ *gocui.View) error {
	return w.moveSelection(1)
}

func (w *ListWidget[T]) onCursorUp(_ *gocui.Gui, _ *gocui.View) error {
	return w.moveSelection(-1)
}

func (w *ListWidget[T]) onSearch(_ *gocui.Gui, v *gocui.View) error {
	return w.nav.OpenSearchModal(v.Name())
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
