// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/awesome-gocui/gocui"
	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/ui/model"
	widgets "github.com/nationallibraryofnorway/warchaeology/v5/internal/ui/widget"
)

const (
	viewFiles        = "Files"
	viewRecords      = "Records"
	viewRecordHeader = "Header"
	viewRecordErrors = "Errors"
	viewChrome       = "Chrome"

	modalShortcuts = "Shortcuts"
	modalSearch    = "Search"
)

type App struct {
	dir      string
	files    []string
	suffixes []string
	tmpDir   string

	gui        *gocui.Gui
	focused    string
	views      []string
	fullscreen bool
	modal      string

	generation uint64
	loadCancel context.CancelFunc
	workers    sync.WaitGroup
	updateSlot chan struct{}

	filesWidget   *widgets.ListWidget[string]
	recordsWidget *widgets.ListWidget[RecordItem]
	recordPanel   *widgets.RecordPanelWidget
	chromeWidget  *widgets.ChromeWidget
	shortcutModal *widgets.ShortcutsModal
	searchModal   *widgets.SearchModal
}

func (a *App) SetSearch(viewName string, q string) {
	switch viewName {
	case viewFiles:
		a.filesWidget.SetFilter(q)
	case viewRecords:
		a.recordsWidget.SetFilter(q)
	}
}

func (a *App) ToggleRecordType(recType gowarc.RecordType) error {
	if recType == 0 {
		a.recordsWidget.SetPredicate(nil)
		return nil
	}
	a.recordsWidget.SetPredicate(func(ri RecordItem) bool {
		return ri.RecordType&recType != 0
	})
	return nil
}

func NewApp(opts *Options) *App {
	dir := opts.Dir
	if dir == "" {
		wd, _ := os.Getwd()
		dir = wd
	}

	return &App{
		dir:        dir,
		focused:    viewFiles,
		updateSlot: make(chan struct{}, 1),
		files:      opts.Files,
		suffixes:   opts.Suffixes,
		tmpDir:     opts.TempDir,
	}
}

func (a *App) Run() error {
	gui, err := gocui.NewGui(gocui.OutputTrue, true)
	if err != nil {
		return err
	}
	defer gui.Close()
	defer a.stopScanning()

	if err := a.setup(gui); err != nil {
		return err
	}
	err = gui.MainLoop()
	if errors.Is(err, gocui.ErrQuit) {
		err = nil
	}
	return err
}

func (a *App) setup(gui *gocui.Gui) error {
	a.gui = gui

	gui.Cursor = false
	gui.Highlight = true
	gui.SupportOverlaps = true
	gui.Mouse = true

	a.filesWidget = widgets.NewListWidget(
		viewFiles,
		func(f string) string { return f },
		a,
		a.onFileSelected,
	)

	a.recordsWidget = widgets.NewListWidget(
		viewRecords,
		func(i RecordItem) string {
			return i.String()
		},
		a,
		a.onRecordSelected,
	)
	a.recordsWidget.SetColorizer(func(ri RecordItem) string {
		return model.RecordTypeFgColor(ri.RecordType)
	})
	a.recordsWidget.SetSubtitleMaker(func(ri RecordItem) string {
		return fmt.Sprintf("Offset: %d", ri.Offset)
	})
	a.recordPanel = widgets.NewRecordPanelWidget(viewRecordHeader, viewRecordErrors, a)
	a.chromeWidget = widgets.NewChromeWidget(gui, viewChrome, a)

	a.shortcutModal = widgets.NewModalShortcuts(modalShortcuts, a)
	a.searchModal = widgets.NewSearchModal(modalSearch, a)

	gui.SetManager(a)
	if err := a.searchModal.Bind(gui); err != nil {
		return err
	}
	if err := a.populateFiles(); err != nil {
		return err
	}
	return a.registerGlobalKeybindings(gui)

}

func (a *App) onShowHelp(_ *gocui.Gui, _ *gocui.View) error {
	a.modal = modalShortcuts
	return nil
}

func (a *App) OpenSearchModal(viewName string) error {
	a.searchModal.SetSourceView(viewName)
	a.modal = modalSearch
	return nil
}

func (a *App) CloseModal(_ *gocui.Gui, _ *gocui.View) error {
	if a.modal != "" {
		_ = a.gui.DeleteView(a.modal)
	}
	a.gui.Cursor = false
	a.modal = ""
	return nil
}

func (a *App) SetCurView(name string) {
	if a.modal == "" && slices.Contains(a.views, name) {
		a.focused = name
	}
}

func (a *App) cycleFocus(step int) error {
	if a.modal == "" && len(a.views) > 0 {
		i := slices.Index(a.views, a.focused)
		a.focused = a.views[(i+step+len(a.views))%len(a.views)]
	}
	return nil
}

func (a *App) onPrevView(*gocui.Gui, *gocui.View) error { return a.cycleFocus(-1) }
func (a *App) onNextView(*gocui.Gui, *gocui.View) error { return a.cycleFocus(1) }

func (a *App) onToggleFullscreen(_ *gocui.Gui, _ *gocui.View) error {
	a.fullscreen = !a.fullscreen
	return nil
}

func (a *App) ToggleLineEndings() error {
	a.recordPanel.ToggleLineEndings()
	item, ok := a.recordsWidget.SelectedItem()
	if !ok {
		return nil
	}
	a.loadRecordDetails(a.gui, item)
	return nil
}

type RecordItem struct {
	Path       string
	ID         string
	RecordType gowarc.RecordType
	Offset     int64
	Err        error // non-nil for placeholder error records
}

func (r RecordItem) String() string {
	if r.Err != nil {
		return r.ID
	}
	return fmt.Sprintf("%s (%s)", r.ID, (r.RecordType &^ model.ErrorRecordType))
}

func (a *App) populateFiles() error {

	if len(a.files) > 0 {
		a.filesWidget.AddElements(a.files)
		return nil
	}

	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return err
	}
	items := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		for _, suffix := range a.suffixes {
			if strings.HasSuffix(entry.Name(), suffix) {
				items = append(items, entry.Name())
				break
			}
		}
	}
	a.filesWidget.AddElements(items)
	return nil
}

func (a *App) registerGlobalKeybindings(gui *gocui.Gui) error {
	quit := func(*gocui.Gui, *gocui.View) error { return gocui.ErrQuit }
	keyBindings := []model.KeyBinding{
		{Key: gocui.KeyCtrlC, Mod: gocui.ModNone, Fn: quit},
		{Key: 'h', Mod: gocui.ModNone, Fn: a.onShowHelp},
		{Key: 'z', Mod: gocui.ModNone, Fn: a.onToggleFullscreen},
		{Key: gocui.KeyCtrlY, Mod: gocui.ModNone, Fn: a.onToggleMouse},
		{Key: gocui.KeyTab, Mod: gocui.ModNone, Fn: a.onNextView},
		{Key: gocui.KeyBacktab, Mod: gocui.ModNone, Fn: a.onPrevView},
	}

	for _, kb := range keyBindings {
		if err := gui.SetKeybinding("", kb.Key, kb.Mod, kb.Fn); err != nil {
			return err
		}
	}

	return nil
}

func (a *App) onToggleMouse(gui *gocui.Gui, _ *gocui.View) error {
	gui.Mouse = !gui.Mouse
	return nil
}

// loadRecordDetails opens the record at item.Offset in path and populates the
// header, content, and errors views.
func (a *App) loadRecordDetails(g *gocui.Gui, item RecordItem) {
	a.recordPanel.Clear(g)
	if item.Err != nil {
		a.recordPanel.RenderReadError(g, item.Err)
		return
	}
	reader, err := gowarc.NewWarcFileReader(item.Path, item.Offset, gowarc.WithBufferTmpDir(a.tmpDir))
	if err != nil {
		a.recordPanel.RenderReadError(g, err)
		return
	}
	defer reader.Close()

	rec, err := reader.Next()
	defer rec.Close()
	if err != nil {
		a.recordPanel.RenderReadError(g, err)
		return
	}
	a.recordPanel.PopulateContent(g, rec)
	a.recordPanel.RenderErrors(g, rec.Validation)
}
