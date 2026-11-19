// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package widget

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/awesome-gocui/gocui"
	"github.com/nationallibraryofnorway/gowarc/v3"
)

var (
	DoubleFrame = []rune{'═', '║', '╔', '╗', '╚', '╝'}
	SingleFrame = []rune{'─', '│', '┌', '┐', '└', '┘'}
)

// RecordPanelWidget displays the selected record and its read or validation errors.
type RecordPanelWidget struct {
	headerName  string
	errorsName  string
	headerTitle string

	header *RecordWidget
	errors *RecordWidget

	// hasErrors controls whether the errors sub-view is included in the layout.
	hasErrors bool
	// errorsOnly hides Header and Content, showing only the errors view.
	errorsOnly bool

	// showLineEndings controls whether CR/LF are rendered as visible escape sequences.
	showLineEndings bool
}

// ToggleLineEndings flips the visible line endings mode on/off.
func (p *RecordPanelWidget) ToggleLineEndings() {
	p.showLineEndings = !p.showLineEndings
}

func NewRecordPanelWidget(headerName, errorsName string, ctrl Controller) *RecordPanelWidget {
	return &RecordPanelWidget{
		headerName:  headerName,
		errorsName:  errorsName,
		headerTitle: "WARC",
		header:      NewRecordWidget(headerName, ctrl),
		errors:      NewRecordWidget(errorsName, ctrl),
	}
}

// ViewNames returns the ordered list of focusable view names in this panel.
// The errors view is only included when errors are present.
func (p *RecordPanelWidget) ViewNames() []string {
	if p.errorsOnly {
		return []string{p.errorsName}
	}
	names := []string{p.headerName}
	if p.hasErrors {
		names = append(names, p.errorsName)
	}
	return names
}

// LayoutPane prepares a persistent view. The screen layout owns its position.
func (p *RecordPanelWidget) LayoutPane(gui *gocui.Gui, name string, x0, y0, x1, y1 int) error {
	w := p.header
	if name == p.errorsName {
		w = p.errors
	}
	if err := w.Layout(gui, x0, y0, x1, y1); err != nil {
		return err
	}
	v, err := gui.View(name)
	if err != nil {
		return err
	}
	if name == p.headerName {
		v.Title = p.headerTitle
	} else if p.errorsOnly {
		v.Title = "Read Error"
	} else {
		v.Title = "Validation Errors"
	}
	return nil
}

// PopulateContent writes header+block into the main record view, rendering
// CR/LF as visible escape sequences when enabled.
func (p *RecordPanelWidget) PopulateContent(gui *gocui.Gui, rec gowarc.Record) {
	v, err := gui.View(p.headerName)
	if err != nil {
		return
	}
	v.Clear()
	_ = v.SetOrigin(0, 0)
	p.headerTitle = fmt.Sprint(rec.WarcRecord.Version())

	warcRecord := rec.WarcRecord
	if _, ok := warcRecord.Block().(gowarc.PayloadBlock); ok {
		if err := warcRecord.Block().Cache(); err != nil {
			_, _ = io.Copy(v, warcHeaderAndSeparatorReader(warcRecord))
			_, _ = fmt.Fprintf(v, "<error caching block: %v>\n", err)
			return
		}
	}

	blockSize := warcRecord.Block().Size()

	r, err := warcRecord.Block().RawBytes()
	if err != nil {
		_, _ = warcRecord.WarcHeader().Write(v)
		_, _ = fmt.Fprint(v, "\r\n")
		_, _ = fmt.Fprintf(v, "<error reading block: %v>\n", err)
		return
	}

	if err := writeContent(v, warcHeaderAndSeparatorReader(warcRecord), r, p.showLineEndings); err != nil {
		_, _ = fmt.Fprintf(v, "<error rendering block: %v>\n", err)
		return
	}

	subtitle := fmt.Sprintf("Record size: %dB, Block size: %dB", rec.Size, blockSize)
	if pb, ok := warcRecord.Block().(gowarc.PayloadBlock); ok {
		if pr, err := pb.PayloadBytes(); err == nil {
			if ps, err := io.Copy(io.Discard, pr); err == nil {
				subtitle = fmt.Sprintf("%s, Payload size: %dB", subtitle, ps)
			}
		}
	}
	v.Subtitle = subtitle
}

func writeContent(dst io.Writer, header, block io.Reader, visible bool) error {
	src := io.MultiReader(header, block)
	if visible {
		src = newVisibleFilter(io.MultiReader(src, strings.NewReader("\r\n\r\n")))
	}
	_, err := io.Copy(dst, src)
	return err
}

func warcHeaderAndSeparatorReader(warcRecord gowarc.WarcRecord) io.Reader {
	var header bytes.Buffer
	_, _ = warcRecord.WarcHeader().Write(&header)
	_, _ = header.WriteString("\r\n")
	return &header
}

// RenderReadError shows a single read error in a full-panel error view,
// hiding the Header and Content views.
func (p *RecordPanelWidget) RenderReadError(gui *gocui.Gui, err error) {
	p.hasErrors = true
	p.errorsOnly = true
	if v, vErr := gui.View(p.errorsName); vErr == nil {
		v.Clear()
		_ = v.SetOrigin(0, 0)
		_, _ = fmt.Fprintf(v, "%v\n", err)
		v.Subtitle = ""
	}
}

// RenderErrors shows validation findings alongside the record.
func (p *RecordPanelWidget) RenderErrors(gui *gocui.Gui, errs []error) {
	p.hasErrors = len(errs) > 0
	if !p.hasErrors {
		return
	}
	if v, err := gui.View(p.errorsName); err == nil {
		v.Clear()
		_ = v.SetOrigin(0, 0)
		for _, e := range errs {
			_, _ = fmt.Fprintf(v, "%v\n", e)
		}
		v.Subtitle = fmt.Sprintf("%d errors", len(errs))
	}
}

// Clear empties all sub-views and hides the errors panel.
func (p *RecordPanelWidget) Clear(gui *gocui.Gui) {
	p.hasErrors = false
	p.errorsOnly = false
	p.headerTitle = "WARC"
	for _, name := range []string{p.headerName, p.errorsName} {
		if v, err := gui.View(name); err == nil {
			v.Clear()
			v.Subtitle = ""
			_ = v.SetOrigin(0, 0)
		}
	}
}
