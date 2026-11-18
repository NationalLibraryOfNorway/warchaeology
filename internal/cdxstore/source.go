package cdxstore

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"
	"uuid"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type generationKey struct{}
type checkpoint struct {
	Generation string           `json:"generation"`
	File       *schema.FileInfo `json:"file"`
}

func (d *DB) BeginFile(ctx context.Context, source string) (context.Context, error) {
	if d.readOnly {
		return nil, ErrReadOnly
	}
	id := uuid.NewV7()
	return context.WithValue(ctx, generationKey{}, id.String()), nil
}

// FinishFile publishes a complete generation through one durable checkpoint.
// A single durable checkpoint publishes a complete generation. Interrupted
// attempts remain invisible. Cleanup is safe to retry after publication.
func (d *DB) FinishFile(ctx context.Context, source string, size int64, modified time.Time) error {
	if d.readOnly {
		return ErrReadOnly
	}
	generation, _ := ctx.Value(generationKey{}).(string)
	if generation == "" {
		return fmt.Errorf("missing file generation")
	}
	d.visibility.Lock()
	defer d.visibility.Unlock()
	cp := checkpoint{Generation: generation, File: &schema.FileInfo{Name: sourceBasename(source), Path: source, Size: size, LastModified: timestamppb.New(modified)}}
	b, e := json.Marshal(cp)
	if e != nil {
		return e
	}
	if e = d.kv.Put(ctx, "s/"+identity(source), b); e != nil {
		return e
	}
	e = d.kv.Scan(ctx, "c/", "c0", func(k string, v []byte) error {
		c := &index.Capture{}
		if e := proto.Unmarshal(v, c); e != nil {
			return e
		}
		if c.GetSource() != source || c.GetGeneration() == generation {
			return nil
		}
		return d.kv.Delete(ctx, k)
	})
	if e != nil {
		return e
	}
	return d.kv.Scan(ctx, "i/", "i0", func(k string, v []byte) error {
		c := &index.Capture{}
		if e := proto.Unmarshal(v, c); e != nil {
			return e
		}
		if c.GetSource() != source || c.GetGeneration() == generation {
			return nil
		}
		return d.kv.Delete(ctx, k)
	})
}

func (d *DB) visible(ctx context.Context, c *index.Capture) (bool, error) {
	if c.GetGeneration() == "" {
		return true, nil
	}
	b, e := d.kv.Get(ctx, "s/"+identity(c.GetSource()))
	if e != nil {
		return false, e
	}
	if len(b) == 0 {
		return false, nil
	}
	var cp checkpoint
	if e = json.Unmarshal(b, &cp); e != nil {
		return false, e
	}
	return cp.Generation == c.GetGeneration(), nil
}

func (d *DB) sourceInfo(ctx context.Context, name string) (*schema.FileInfo, error) {
	var found *schema.FileInfo
	e := d.kv.Scan(ctx, "s/", "s0", func(_ string, b []byte) error {
		var cp checkpoint
		if e := json.Unmarshal(b, &cp); e != nil {
			return e
		}
		f := cp.File
		if f == nil {
			return fmt.Errorf("invalid file checkpoint")
		}
		if f.GetPath() == name || sourceBasename(f.GetPath()) == name {
			if found != nil && found.GetPath() != f.GetPath() {
				return fmt.Errorf("ambiguous filename %q; use source locator", name)
			}
			found = f
		}
		return nil
	})
	return found, e
}

func sourceBasename(source string) string {
	var s struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(source), &s) == nil {
		return filepath.Base(s.Path)
	}
	return filepath.Base(source)
}
