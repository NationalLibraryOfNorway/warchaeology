// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
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

// CaptureByTarget resolves an exact revisit target without selecting a nearby capture.
func (d *DB) CaptureByTarget(ctx context.Context, uri string, date time.Time) (capture *cdx.Capture, err error) {
	key := cdx.CanonicalKey(uri) + " "
	err = d.Scan(ctx, key, key+"\xff", func(c *cdx.Capture) error {
		if c.GetUri() != uri || !c.GetSts().AsTime().Equal(date) || c.GetRef() == "" {
			return nil
		}
		if capture != nil && capture.GetRef() != c.GetRef() {
			return fmt.Errorf("ambiguous revisit target")
		}
		capture = c
		return nil
	})
	return capture, err
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
