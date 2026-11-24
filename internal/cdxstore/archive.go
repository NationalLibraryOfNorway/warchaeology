// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
)

var ErrArchiveChanged = errors.New("archive source changed; reindex before replay")

type ArchiveInfo struct {
	*schema.FileInfo

	Generation string
	CRC        *uint32
	Container  *filewalker.FileVersion
}

// ArchiveSource resolves an opaque public file ID to a registered native source.
// Callers cannot use this API to open an arbitrary filesystem path.
func (d *DB) ArchiveSource(ctx context.Context, id string) (ArchiveInfo, error) {
	if len(id) != 64 {
		return ArchiveInfo{}, os.ErrNotExist
	}
	if _, err := hex.DecodeString(id); err != nil {
		return ArchiveInfo{}, os.ErrNotExist
	}
	raw, err := d.kv.Get(ctx, "s/"+id)
	if err != nil {
		return ArchiveInfo{}, err
	}
	if len(raw) == 0 {
		return ArchiveInfo{}, os.ErrNotExist
	}
	var cp checkpoint
	if err = json.Unmarshal(raw, &cp); err != nil {
		return ArchiveInfo{}, err
	}
	if cp.File.GetPath() == "" {
		return ArchiveInfo{}, fmt.Errorf("source checkpoint has no file")
	}
	return ArchiveInfo{FileInfo: cp.File, Generation: cp.Generation, CRC: cp.CRC, Container: cp.Container}, nil
}

func ArchiveID(source string) string { return identity(source) }

// ValidateArchiveSource checks the opened member and its outer local container.
func (d *DB) ValidateArchiveSource(ctx context.Context, source, generation string, actual fs.FileInfo) error {
	info, err := d.ArchiveSource(ctx, ArchiveID(source))
	if err != nil {
		return err
	}
	expected := filewalker.FileVersion{Size: info.GetSize(), Modified: info.GetLastModified().AsTime(), CRC: info.CRC}
	if (generation != "" && generation != info.Generation) || !expected.Matches(filewalker.VersionOf(actual)) {
		return ErrArchiveChanged
	}
	current, present, err := filewalker.ContainerVersion(source)
	if err != nil {
		return err
	}
	if present && (info.Container == nil || !info.Container.Matches(current)) {
		return ErrArchiveChanged
	}
	return nil
}

// Key by the original WARC URI and second, independently of the replay key.
// Embedded CDX timestamps can omit the WARC's fractional seconds.
func targetPrefix(uri string, date time.Time) string {
	return "target/" + identity(uri) + "/" + timestamp.Format14(date) + "/"
}

// Ordinary native rows are already searchable by original URI and second.
func needsTargetKey(c *cdx.Capture) bool {
	return c.GetRef() != "" && c.GetSsu() != cdx.CanonicalKey(c.GetUri())
}

func targetKey(c *cdx.Capture, key string) string {
	return targetPrefix(c.GetUri(), c.GetSts().AsTime()) + identity(key)
}

// Existing read-only catalogues can resolve targets by scanning until their
// next writable open backfills the secondary index.
func (d *DB) openTargetIndex(ctx context.Context) error {
	if d.indexOnly {
		return nil
	}
	marker, err := d.kv.Get(ctx, "!target-index")
	if err != nil {
		return err
	}
	d.targetIndexed = len(marker) > 0
	if d.targetIndexed || d.readOnly {
		return nil
	}
	var ops []mutation
	err = d.kv.Scan(ctx, "c/", "c0", func(key string, raw []byte) error {
		c, err := decodeCapture(key, raw)
		if err != nil {
			return err
		}
		if !needsTargetKey(c) {
			return nil
		}
		ops = append(ops, mutation{key: targetKey(c, key), value: []byte(key)})
		if len(ops) < 128 && batchBytes(ops) < 1<<20 {
			return nil
		}
		err = d.kv.batch(ctx, ops)
		ops = ops[:0]
		return err
	})
	if err != nil {
		return err
	}
	ops = append(ops, mutation{key: "!target-index", value: []byte{1}})
	if err := d.kv.batch(ctx, ops); err != nil {
		return err
	}
	d.targetIndexed = true
	return nil
}

// CapturesByTarget returns all physical candidates for an exact revisit target.
func (d *DB) CapturesByTarget(ctx context.Context, uri string, date time.Time) ([]*cdx.Capture, error) {
	var candidates []*cdx.Capture
	err := d.VisitCapturesByTarget(ctx, uri, date, func(c *cdx.Capture) error {
		candidates = append(candidates, c)
		return nil
	})
	return candidates, err
}

// VisitCapturesByTarget streams candidates without retaining all captures in one second.
func (d *DB) VisitCapturesByTarget(ctx context.Context, uri string, date time.Time, emit func(*cdx.Capture) error) error {
	collect := func(c *cdx.Capture) error {
		stamp := c.GetSts().AsTime()
		if c.GetUri() == uri && c.GetRef() != "" && (stamp.Equal(date) || stamp.Equal(date.Truncate(time.Second))) {
			return emit(c)
		}
		return nil
	}
	if !d.targetIndexed {
		return d.Scan(ctx, "", "", collect)
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return err
	}
	defer done()
	d.visibility.RLock()
	defer d.visibility.RUnlock()
	key := cdx.CanonicalKey(uri) + " " + timestamp.Format14(date) + "."
	if err := d.scan(ctx, key, key+"\xff", collect); err != nil {
		return err
	}
	return d.scanCaptureRefs(ctx, targetPrefix(uri, date), collect)
}

// CaptureByID supplies the generation alongside the physical record location.
func (d *DB) CaptureByID(ctx context.Context, id string) (capture *cdx.Capture, err error) {
	err = d.CapturesByID(ctx, id, func(c *cdx.Capture) error {
		if capture != nil && capture.GetRef() != c.GetRef() {
			return fmt.Errorf("ambiguous record ID")
		}
		capture = c
		return nil
	})
	return capture, err
}
