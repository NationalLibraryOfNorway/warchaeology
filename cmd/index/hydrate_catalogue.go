// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nationallibraryofnorway/gowarc/v3"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcdigest"
)

// hydrateLookupError distinguishes catalogue failures from unverifiable WARCs.
type hydrateLookupError struct{ error }

func (e hydrateLookupError) Unwrap() error { return e.error }

type hydrateCatalogue struct {
	input, native, ids *cdxstore.DB
	tmp                string
	archives           mountedWARCs
	unreadableID       error
}

func openHydrateCatalogue(ctx context.Context, tmp, donorDir string, archives mountedWARCs) (*hydrateCatalogue, error) {
	input, err := openHydrateScratch(ctx, tmp, "targets")
	if err != nil {
		return nil, err
	}
	c := &hydrateCatalogue{input: input, tmp: tmp, archives: archives}
	if donorDir != "" {
		c.native, err = cdxstore.Open(ctx, cdxstore.Options{Format: badgerFormat, Dir: donorDir, ReadOnly: true})
		if err != nil {
			return nil, errors.Join(fmt.Errorf("open donor catalogue: %w", err), c.close())
		}
	}
	return c, nil
}

func openHydrateScratch(ctx context.Context, tmp, name string) (*cdxstore.DB, error) {
	return cdxstore.Open(ctx, cdxstore.Options{Format: badgerFormat, Dir: filepath.Join(tmp, name), Scratch: true})
}

func (h *hydrateCatalogue) close() (err error) {
	for _, db := range []*cdxstore.DB{h.ids, h.native, h.input} {
		if db != nil {
			err = errors.Join(err, db.Close())
		}
	}
	return err
}

func (h *hydrateCatalogue) add(ctx context.Context, c *cdx.Capture) error {
	h.archives.locate(c, true)
	c.Dig = replayDigest(c.GetDig())
	c.ExtraJson = nil
	// Keep distinct catalogue claims about the same position. Equivalent rows
	// share a lookup entry, and the spool retains every occurrence for output.
	identity := fmt.Sprintf("%q %q %q %d", c.GetSsu(), c.GetUri(), c.GetDig(), c.GetRle())
	c.Ssu = fmt.Sprintf("hydrate:%x", sha256.Sum256([]byte(identity)))
	return h.input.WriteCapture(ctx, c)
}

func (h *hydrateCatalogue) capturesByTarget(ctx context.Context, uri string, date time.Time, emit func(*cdx.Capture) error) error {
	if err := h.input.VisitCapturesByTarget(ctx, uri, date, emit); err != nil {
		return hydrateLookupError{err}
	}
	if h.native != nil {
		err := h.native.VisitCapturesByTarget(ctx, uri, date, func(c *cdx.Capture) error {
			h.archives.locate(c, true)
			return emit(c)
		})
		if err != nil {
			return hydrateLookupError{err}
		}
	}
	return nil
}

func (h *hydrateCatalogue) CapturesByID(ctx context.Context, id string, emit func(*cdx.Capture) error) error {
	if h.ids == nil {
		if err := h.indexMissingIDs(ctx); err != nil {
			return hydrateLookupError{err}
		}
	}
	found := false
	collect := func(c *cdx.Capture) error {
		found = true
		return emit(c)
	}
	if err := h.ids.CapturesByID(ctx, id, collect); err != nil {
		return hydrateLookupError{err}
	}
	if h.native != nil {
		err := h.native.CapturesByID(ctx, id, func(c *cdx.Capture) error {
			h.archives.locate(c, true)
			return collect(c)
		})
		if err != nil {
			return hydrateLookupError{err}
		}
	}
	if !found && h.unreadableID != nil {
		return fmt.Errorf("record ID %q could not be found, and some indexed WARC headers could not be read: %w", id, h.unreadableID)
	}
	return nil
}

func (h *hydrateCatalogue) indexMissingIDs(ctx context.Context) error {
	ids, err := openHydrateScratch(ctx, h.tmp, "ids")
	if err != nil {
		return err
	}
	h.ids = ids
	for _, db := range []*cdxstore.DB{h.input, h.native} {
		if db == nil {
			continue
		}
		err = db.Scan(ctx, "", "", func(c *cdx.Capture) error {
			if c.GetRid() != "" || c.GetRef() == "" {
				return nil
			}
			if db == h.native {
				h.archives.locate(c, true)
			}
			id, err := h.archives.recordID(ctx, c)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if h.unreadableID == nil {
					h.unreadableID = fmt.Errorf("%s at %d: %w", c.GetFilename(), c.GetOffset(), err)
				}
				return nil
			}
			c.Rid, c.ExtraJson, c.Generation = id, nil, ""
			return ids.WriteCapture(ctx, c)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type mountedWARCs struct {
	root *os.Root
	path string
}

func (m mountedWARCs) matchesOriginal(ctx context.Context, fields map[string]any, donor *cdx.Capture) bool {
	name, _ := fields["orig.filename"].(string)
	if fields["mime"] != donor.GetMct() || name == "-" || !filepath.IsLocal(filepath.FromSlash(name)) {
		return false
	}
	offset, offsetErr := cdxNumber(fields["orig.offset"])
	length, lengthErr := cdxNumber(fields["orig.length"])
	if offsetErr != nil || lengthErr != nil || offset < 0 || length <= 0 {
		return false
	}
	if name == donor.GetFilename() && offset == donor.GetOffset() && length == donor.GetRle() {
		return true
	}

	// A byte-equivalent copy of the selected response is also a valid original.
	alternate := &cdx.Capture{Filename: name, Offset: offset, Rle: length, Uri: donor.GetUri(), Sts: donor.GetSts()}
	m.locate(alternate, false)
	other, err := loader.CaptureFingerprint(ctx, m, alternate)
	if err != nil {
		return false
	}
	verified, err := loader.CaptureFingerprint(ctx, m, donor)
	return err == nil && verified == other
}

// locate maps native source paths on this mount, falling back to the public
// filename for a relocated catalogue. Input rows always use their public name.
func (m mountedWARCs) locate(c *cdx.Capture, normalizeFilename bool) {
	name := filepath.FromSlash(c.GetFilename())
	source := c.GetSource()
	if strings.HasPrefix(source, "{") {
		var locator filewalker.Source
		if err := json.Unmarshal([]byte(source), &locator); err == nil && len(locator.Roots) == 0 {
			source = locator.Path
		}
	}
	if filepath.IsAbs(source) && insidePath(m.path, source) {
		name = source
	}
	if filepath.IsAbs(name) && insidePath(m.path, name) {
		name, _ = filepath.Rel(m.path, name)
	}
	c.Source = filepath.ToSlash(filepath.Clean(name))
	c.Ref = "warcfile:" + c.GetSource() + "#" + strconv.FormatInt(c.GetOffset(), 10)
	if normalizeFilename {
		c.Filename = c.GetSource()
	}
}

func (m mountedWARCs) open(name string) (*os.File, int64, error) {
	name = filepath.FromSlash(name)
	if name == "-" || !filepath.IsLocal(name) {
		return nil, 0, fmt.Errorf("archive filename %q is outside the WARC root or missing", name)
	}
	// Stat before open so an accidental FIFO cannot block an offline run.
	info, err := m.root.Stat(name)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("archive filename %q is not a regular file", name)
	}
	f, err := m.root.Open(name)
	return f, info.Size(), err
}

// recordID reads only the WARC header at the indexed position. The complete
// record is opened and verified by the shared resolver if it becomes a donor.
func (m mountedWARCs) recordID(ctx context.Context, c *cdx.Capture) (_ string, err error) {
	f, size, err := m.open(c.GetSource())
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if c.GetOffset() < 0 || c.GetOffset() >= size {
		return "", fmt.Errorf("offset outside archive")
	}
	buffer := bufio.NewReader(io.NewSectionReader(archiveReader{ctx, f}, c.GetOffset(), size-c.GetOffset()))
	var reader io.Reader = buffer
	magic, _ := buffer.Peek(2)
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(buffer)
		if err != nil {
			return "", err
		}
		defer func() { err = errors.Join(err, gz.Close()) }()
		gz.Multistream(false)
		reader = gz
	}
	header := textproto.NewReader(bufio.NewReader(reader))
	version, err := header.ReadLine()
	if err != nil || !strings.HasPrefix(version, "WARC/") {
		return "", fmt.Errorf("no WARC header at indexed offset %d", c.GetOffset())
	}
	fields, err := header.ReadMIMEHeader()
	if err != nil {
		return "", err
	}
	id := strings.Trim(fields.Get("WARC-Record-ID"), "<>")
	if id == "" {
		return "", fmt.Errorf("indexed WARC record has no record ID")
	}
	return id, nil
}

func (m mountedWARCs) RecordByStorageRef(ctx context.Context, ref string) (gowarc.WarcRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c := loader.CaptureFromContext(ctx)
	name, offset, err := cdx.ParseStorageRef(ref)
	if err != nil {
		return nil, err
	}
	f, size, err := m.open(name)
	if err != nil {
		return nil, err
	}
	var record gowarc.Record
	err = func() error {
		if offset < 0 || offset >= size || c.GetRle() <= 0 || c.GetRle() > size-offset {
			return fmt.Errorf("indexed record range outside archive")
		}
		r, err := gowarc.NewWarcFileReaderFromStream(io.NewSectionReader(archiveReader{ctx, f}, 0, size), offset,
			gowarc.WithSyntaxErrorPolicy(gowarc.ErrIgnore), gowarc.WithSpecViolationPolicy(gowarc.ErrWarn))
		if err != nil {
			return err
		}
		record, err = r.Next()
		if err = errors.Join(err, r.Close()); err != nil {
			return err
		}
		if record.Offset != offset {
			return fmt.Errorf("no WARC record at indexed offset %d", offset)
		}
		length, err := storedLength(f, record)
		if err != nil {
			return err
		}
		if length != c.GetRle() {
			return fmt.Errorf("indexed record length %d does not match WARC record length %d", c.GetRle(), length)
		}
		return checkMountedCapture(c, record.WarcRecord)
	}()
	if err = errors.Join(err, f.Close()); err != nil {
		return nil, fmt.Errorf("%s at %d: %w", name, offset, errors.Join(err, record.Close()))
	}
	return record.WarcRecord, nil
}

type archiveReader struct {
	ctx context.Context
	io.ReaderAt
}

func (r archiveReader) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReaderAt.ReadAt(p, offset)
}

func checkMountedCapture(c *cdx.Capture, record gowarc.WarcRecord) error {
	header := record.WarcHeader()
	if header.Get(gowarc.WarcTargetURI) != c.GetUri() {
		return fmt.Errorf("indexed URL does not match WARC target URI")
	}
	actual, err := header.GetTime(gowarc.WarcDate)
	if err != nil {
		return err
	}
	indexed := c.GetSts().AsTime()
	if indexed.Nanosecond() == 0 {
		actual = actual.Truncate(time.Second)
	}
	if !indexed.Equal(actual) {
		return fmt.Errorf("indexed timestamp does not match WARC date")
	}
	if digest := replayDigest(c.GetDig()); record.Type() == gowarc.Revisit && digest != "" &&
		!warcdigest.Equal(digest, header.Get(gowarc.WarcPayloadDigest)) {
		return fmt.Errorf("indexed revisit digest does not match WARC payload digest")
	}
	return nil
}

func replayDigest(digest string) string {
	if digest == "-" || digest == "" {
		return ""
	}
	if !strings.Contains(digest, ":") {
		return "sha1:" + digest
	}
	return digest
}
