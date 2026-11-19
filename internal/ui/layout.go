// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"errors"
	"slices"

	"github.com/awesome-gocui/gocui"
	widgets "github.com/nationallibraryofnorway/warchaeology/v5/internal/ui/widget"
)

const (
	filesViewHeight  = 9
	recordsListWidth = 51
	recordPanelX0    = recordsListWidth + 1
)

// Coordinates include the frame. The footer's unframed content occupies the
// last terminal row, between its two (undrawn) horizontal borders.
type rect struct{ left, top, right, bottom int }

func paneBounds(width, height, files int, details []string, focus string, fullscreen bool) map[string]rect {
	panes := make(map[string]rect)
	if width < 3 || height < 3 {
		return panes
	}
	// Reserve the bottom row for shortcuts.
	panes[viewChrome] = rect{0, height - 2, width - 1, height}
	body := rect{0, 0, width - 1, height - 2}
	filesBottom := min(filesViewHeight, max(1, files)+1)
	lowerTop := filesBottom + 1
	// Each detail view needs two borders and at least one content row.
	if fullscreen || width < recordPanelX0+3 || body.bottom-lowerTop+1 < 3*len(details) {
		panes[focus] = body
		return panes
	}
	panes[viewFiles] = rect{0, 0, body.right, filesBottom}
	panes[viewRecords] = rect{0, lowerTop, recordsListWidth, body.bottom}
	detail := rect{recordPanelX0, lowerTop, body.right, body.bottom}
	if len(details) == 2 {
		errorRows := max(3, (detail.bottom-detail.top+1)/4)
		errorsTop := detail.bottom - errorRows + 1
		panes[viewRecordErrors] = rect{detail.left, errorsTop, detail.right, detail.bottom}
		detail.bottom = errorsTop - 1
	}
	panes[details[0]] = detail
	return panes
}

func (a *App) Layout(gui *gocui.Gui) error {
	width, height := gui.Size()
	return a.layoutAt(gui, width, height)
}

func (a *App) layoutAt(gui *gocui.Gui, width, height int) error {
	details := a.recordPanel.ViewNames()
	a.views = append([]string{viewFiles, viewRecords}, details...)
	if !slices.Contains(a.views, a.focused) {
		a.focused = details[0]
	}
	bounds := paneBounds(width, height, a.filesWidget.VisibleCount(), details, a.focused, a.fullscreen)
	for _, name := range []string{viewFiles, viewRecords, viewRecordHeader, viewRecordErrors, viewChrome} {
		box, visible := bounds[name]
		if !visible {
			if v, err := gui.View(name); err == nil && !v.Visible {
				continue
			}
			// Keep buffers and bindings alive. gocui also hit-tests hidden views,
			// so park their frames outside the screen.
			box = rect{-4, -4, -1, -1}
		}
		if err := a.placePane(gui, name, box); err != nil {
			return err
		}
		v, err := gui.View(name)
		if err != nil {
			return err
		}
		v.Visible = visible
		if name == viewFiles {
			v.Title = a.dir
		}
		v.FrameRunes = widgets.SingleFrame
		if name == a.focused && a.modal == "" {
			v.FrameRunes = widgets.DoubleFrame
		}
	}
	if a.modal != "" {
		return a.placeDialog(gui, width, height)
	}
	_, err := gui.SetCurrentView(a.focused)
	return err
}

func (a *App) placePane(gui *gocui.Gui, name string, r rect) error {
	var w widgets.Widget
	switch name {
	case viewFiles:
		w = a.filesWidget
	case viewRecords:
		w = a.recordsWidget
	case viewChrome:
		w = a.chromeWidget
	default:
		return a.recordPanel.LayoutPane(gui, name, r.left, r.top, r.right, r.bottom)
	}
	return w.Layout(gui, r.left, r.top, r.right, r.bottom)
}

func (a *App) placeDialog(gui *gocui.Gui, width, height int) error {
	w, h := 61, 31
	var dialog widgets.Modal = a.shortcutModal
	if a.modal == modalSearch {
		w, h = 71, 3
		dialog = a.searchModal
	}
	w, h = max(2, min(w, width)), max(2, min(h, height))
	x, y := max(0, (width-w)/2), max(0, (height-h)/2)
	_, err := gui.SetView(a.modal, x, y, x+w-1, y+h-1, 0)
	if errors.Is(err, gocui.ErrUnknownView) {
		err = dialog.Init(gui)
	}
	if err != nil {
		return err
	}
	if _, err = gui.SetViewOnTop(a.modal); err != nil {
		return err
	}
	_, err = gui.SetCurrentView(a.modal)
	return err
}
