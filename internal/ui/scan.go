// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/awesome-gocui/gocui"
	"github.com/nationallibraryofnorway/gowarc/v4"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/ui/model"
)

const recordBatchSize = 97

func (a *App) cancelScan() {
	a.generation++
	if a.loadCancel != nil {
		a.loadCancel()
		a.loadCancel = nil
	}
}

func (a *App) stopScanning() {
	a.cancelScan()
	a.workers.Wait()
}

func (a *App) onFileSelected(name string, ok bool) {
	a.cancelScan()
	a.recordsWidget.Clear()
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.loadCancel = cancel
	path, generation := filepath.Join(a.dir, name), a.generation
	a.workers.Go(func() {
		defer cancel()
		err := scanRecords(ctx, path, a.tmpDir, func(batch []RecordItem) bool {
			return a.postScan(ctx, generation, batch, nil)
		})
		if err != nil {
			a.postScan(ctx, generation, nil, err)
		}
	})
}

// At most one scan update can occupy the GUI queue. Waiting for that slot
// is cancellable even after the main loop has stopped consuming events.
func (a *App) postScan(ctx context.Context, generation uint64, batch []RecordItem, err error) bool {
	select {
	case <-ctx.Done():
		return false
	case a.updateSlot <- struct{}{}:
	}
	a.gui.UpdateAsync(func(*gocui.Gui) error {
		<-a.updateSlot
		a.applyScan(generation, batch, err)
		return nil
	})
	return true
}

func (a *App) applyScan(generation uint64, batch []RecordItem, err error) {
	if generation != a.generation {
		return
	}
	if err != nil {
		a.recordPanel.RenderReadError(a.gui, err)
		return
	}
	a.recordsWidget.AddElements(batch)
}

func (a *App) onRecordSelected(item RecordItem, ok bool) {
	if ok {
		a.loadRecordDetails(a.gui, item)
	} else {
		a.recordPanel.Clear(a.gui)
	}
}

func scanRecords(ctx context.Context, path, tmpDir string, publish func([]RecordItem) bool) (err error) {
	reader, err := gowarc.NewWarcFileReader(path, 0, gowarc.WithBufferTmpDir(tmpDir))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	var batch []RecordItem
	for rec, err := range reader.Records() {
		if ctx.Err() != nil {
			return rec.Close()
		}
		item := RecordItem{Path: path, Offset: rec.Offset}
		if err != nil {
			item.ID = "<read error>"
			item.Err = err
			item.RecordType = model.ErrorRecordType
		} else {
			item.ID = rec.WarcRecord.WarcHeader().Get(gowarc.WarcRecordID)
			item.RecordType = rec.WarcRecord.Type()
			if len(rec.Validation) > 0 {
				item.RecordType |= model.ErrorRecordType
			}
		}
		if closeErr := rec.Close(); closeErr != nil {
			return errors.Join(err, closeErr)
		}
		batch = append(batch, item)
		if len(batch) == recordBatchSize {
			if !publish(batch) {
				return nil
			}
			batch = nil
		}
	}
	if len(batch) > 0 && ctx.Err() == nil {
		publish(batch)
	}
	return nil
}
