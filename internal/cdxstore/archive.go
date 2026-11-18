package cdxstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
)

// ArchiveSource resolves an opaque public file ID to a registered native source.
// Callers cannot use this API to open an arbitrary filesystem path.
func (d *DB) ArchiveSource(ctx context.Context, id string) (*schema.FileInfo, string, error) {
	if len(id) != 64 {
		return nil, "", os.ErrNotExist
	}
	if _, err := hex.DecodeString(id); err != nil {
		return nil, "", os.ErrNotExist
	}
	raw, err := d.kv.Get(ctx, "s/"+id)
	if err != nil {
		return nil, "", err
	}
	if len(raw) == 0 {
		return nil, "", os.ErrNotExist
	}
	var cp checkpoint
	if err = json.Unmarshal(raw, &cp); err != nil {
		return nil, "", err
	}
	if cp.File == nil || cp.File.Path == "" {
		return nil, "", fmt.Errorf("source checkpoint has no file")
	}
	return cp.File, cp.Generation, nil
}

func ArchiveID(source string) string { return identity(source) }
