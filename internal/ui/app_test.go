// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awesome-gocui/gocui"
)

func simulator(t *testing.T) (*App, *gocui.Gui) {
	t.Helper()
	gui, err := gocui.NewGui(gocui.OutputSimulator, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gui.Close)
	app := NewApp(&Options{Dir: t.TempDir()})
	t.Cleanup(app.stopScanning)
	if err := app.setup(gui); err != nil {
		t.Fatal(err)
	}
	return app, gui
}

func TestResizeAndFocus(t *testing.T) {
	a, g := simulator(t)
	layout := func(w, h int) {
		t.Helper()
		if err := a.layoutAt(g, w, h); err != nil {
			t.Fatal(err)
		}
	}
	visible := func(name string) bool {
		t.Helper()
		v, err := g.View(name)
		if err != nil {
			t.Fatal(err)
		}
		return v.Visible
	}
	layout(80, 10)
	if !visible(viewRecordHeader) {
		t.Fatal("details should fit below a one-row file list")
	}
	layout(40, 10)
	if !visible(viewFiles) || visible(viewRecordHeader) {
		t.Fatal("expected compact file list")
	}
	a.cycleFocus(1)
	layout(40, 10)
	if !visible(viewRecords) || g.CurrentView().Name() != viewRecords {
		t.Fatal("Tab should reveal Records")
	}
	a.OpenSearchModal(viewRecords)
	layout(28, 8)
	x, y, right, bottom := g.CurrentView().Dimensions()
	if g.CurrentView().Name() != modalSearch || x < 0 || right >= 28 || y < 0 || bottom >= 8 {
		t.Fatal("search does not fit")
	}
	a.CloseModal(g, nil)
	layout(80, 24)
	if g.CurrentView().Name() != viewRecords || !visible(viewRecordHeader) {
		t.Fatal("split or focus lost after resize")
	}
	a.onToggleFullscreen(g, nil)
	layout(80, 24)
	if visible(viewRecordHeader) || !visible(viewRecords) {
		t.Fatal("expected fullscreen records")
	}
	a.onToggleFullscreen(g, nil)
	a.applyScan(a.generation, nil, errors.New("file disappeared"))
	layout(80, 24)
	a.SetCurView(viewRecordErrors)
	a.recordPanel.Clear(g)
	layout(80, 24)
	if g.CurrentView().Name() != viewRecordHeader {
		t.Fatal("expected focus to return to details")
	}
}

func TestOnlyCurrentFileSelectionCanPublish(t *testing.T) {
	a, g := simulator(t)
	if err := a.layoutAt(g, 80, 10); err != nil {
		t.Fatal(err)
	}
	a.filesWidget.AddElements([]string{"a.warc", "b.warc"})
	first := a.generation
	a.SetSearch(viewFiles, "b.warc")
	second := a.generation
	a.SetSearch(viewFiles, "a.warc")
	a.applyScan(first, []RecordItem{{ID: "obsolete"}}, nil)
	a.applyScan(second, nil, errors.New("obsolete failure"))
	if a.recordsWidget.VisibleCount() != 0 || a.recordPanel.ViewNames()[0] == viewRecordErrors {
		t.Fatal("accepted stale scan")
	}

	path := filepath.Join("..", "..", "testdata", "warc", "single-record.warc")
	err := scanRecords(context.Background(), path, t.TempDir(), func(batch []RecordItem) bool {
		a.applyScan(a.generation, batch, nil)
		return true
	})
	item, ok := a.recordsWidget.SelectedItem()
	if err != nil || !ok || item.Path != path {
		t.Fatalf("selection %v, scan error %v", item, err)
	}
	header, err := g.View(viewRecordHeader)
	if err != nil || !strings.Contains(header.Buffer(), item.ID) {
		t.Fatal("detail does not match selected record")
	}
	current := a.generation
	a.SetSearch(viewFiles, "no matches")
	a.applyScan(current, []RecordItem{item}, nil)
	if a.recordsWidget.VisibleCount() != 0 || strings.Contains(header.Buffer(), item.ID) {
		t.Fatal("stale record after clearing selection")
	}
}

func TestScanOpenFailureAndEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.warc")
	noRecords := func([]RecordItem) bool { t.Fatal("unexpected record"); return false }
	if err := scanRecords(context.Background(), path, dir, noRecords); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open: %v", err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := scanRecords(context.Background(), path, dir, noRecords); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownReleasesWaitingScanner(t *testing.T) {
	a, _ := simulator(t)
	ctx, cancel := context.WithCancel(context.Background())
	a.loadCancel = cancel
	// Leave the last UI event undelivered, as happens when Ctrl+C wins.
	if !a.postScan(ctx, a.generation, nil, nil) {
		t.Fatal("first batch was dropped")
	}
	generation := a.generation
	started := make(chan struct{})
	delivered := make(chan bool, 1)
	a.workers.Go(func() {
		close(started)
		delivered <- a.postScan(ctx, generation, nil, nil)
	})
	<-started
	a.stopScanning()
	if <-delivered {
		t.Fatal("delivered a batch after shutdown")
	}
}
