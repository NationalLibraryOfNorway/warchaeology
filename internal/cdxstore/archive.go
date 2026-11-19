// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
)

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

var ErrArchiveChanged = errors.New("archive source changed; reindex before replay")
