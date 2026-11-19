// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/storagecodec"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type generationKey struct{}
type checkpoint struct {
	Generation string           `json:"generation"`
	Count      int64            `json:"count"`
	File       *schema.FileInfo `json:"file"`
}

type generation struct {
	mu         sync.Mutex
	published  bool
	id, source string
	ops        []mutation
	count      int64
	size       int
}

func (g *generation) flush(ctx context.Context, d *DB) error {
	if len(g.ops) == 0 {
		return nil
	}
	if e := d.batch(ctx, g.ops); e != nil {
		return e
	}
	g.ops = nil
	g.size = 0
	return nil
}
func (g *generation) write(ctx context.Context, d *DB, c *index.Capture) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.published {
		return fmt.Errorf("file generation already published")
	}
	if c.GetSource() != g.source {
		return fmt.Errorf("source does not match generation")
	}
	c.Generation = g.id
	b, e := storagecodec.Encode(c)
	if e != nil {
		return e
	}
	key := captureKey(c)
	g.ops = append(g.ops, mutation{key: key, value: b}, mutation{key: "f/" + identity(g.source) + "/" + g.id + "/" + fmt.Sprintf("%020d", c.GetOffset()), value: []byte(key)})
	if !d.indexOnly && c.GetRid() != "" {
		g.ops = append(g.ops, mutation{key: recordKey(c), value: []byte(key)})
	}
	g.count++
	g.size += len(b) + len(key)*2
	if len(g.ops) >= 256 || g.size >= 1<<20 {
		return g.flush(ctx, d)
	}
	return ctx.Err()
}
func (d *DB) DiscardFile(ctx context.Context) error {
	g, ok := ctx.Value(generationKey{}).(*generation)
	if !ok {
		return fmt.Errorf("missing file generation")
	}
	if g.published {
		return nil
	}
	return d.cleanGeneration(context.WithoutCancel(ctx), generationMarker(g.source, g.id))
}

func (d *DB) BeginFile(ctx context.Context, source string) (context.Context, error) {
	if d.readOnly {
		return nil, ErrReadOnly
	}
	g := &generation{id: uuid.NewV7().String(), source: source}
	if err := d.kv.Put(ctx, generationMarker(source, g.id), []byte{1}); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, generationKey{}, g), nil
}

// FinishFile publishes bounded durable batches with a single source checkpoint.
func (d *DB) FinishFile(ctx context.Context, source string, size int64, modified time.Time) error {
	if d.readOnly {
		return ErrReadOnly
	}
	g, ok := ctx.Value(generationKey{}).(*generation)
	if !ok || g.source != source {
		return fmt.Errorf("missing file generation")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.published {
		return nil
	}
	if e := g.flush(ctx, d); e != nil {
		return e
	}
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	d.visibility.Lock()
	defer d.visibility.Unlock()
	if e := d.recoverUploads(ctx); e != nil {
		return e
	}
	var previous checkpoint
	raw, e := d.kv.Get(ctx, "s/"+identity(source))
	if e != nil {
		return e
	}
	if len(raw) > 0 {
		if e = json.Unmarshal(raw, &previous); e != nil {
			return e
		}
	}
	// Deletions may have removed published native captures since the checkpoint.
	previous.Count = 0
	if previous.Generation != "" {
		prefix := "f/" + identity(source) + "/" + previous.Generation + "/"
		if e = d.kv.Scan(ctx, prefix, prefix+"\xff", func(_ string, ref []byte) error {
			b, e := d.kv.Get(ctx, string(ref))
			if e != nil {
				return e
			}
			if len(b) > 0 {
				previous.Count++
			}
			return nil
		}); e != nil {
			return e
		}
	}
	cp := checkpoint{
		Generation: g.id,
		Count:      g.count,
		File: &schema.FileInfo{
			Name:         sourceBasename(source),
			Path:         source,
			Size:         size,
			LastModified: timestamppb.New(modified),
		},
	}
	if d.indexOnly {
		cp.File = &schema.FileInfo{Path: source, Size: size, LastModified: timestamppb.New(modified)}
	}
	b, e := json.Marshal(cp)
	if e != nil {
		return e
	}
	count, e := d.count(ctx)
	if e != nil {
		return e
	}
	overlay := "upload-native-" + g.id
	defer func() {
		if !g.published {
			_ = d.cleanUpload(context.WithoutCancel(ctx), overlay, false)
		}
	}()
	replaced, e := d.supersedeImports(ctx, g, overlay)
	if e != nil {
		return e
	}
	ops := []mutation{{key: generationMarker(source, g.id), remove: true}, {key: "s/" + identity(source), value: b}, {key: countKey, value: []byte(strconv.FormatInt(count-previous.Count+g.count-replaced, 10))}, {key: "u/" + overlay, value: []byte{1}}}
	if previous.Generation != "" {
		ops = append(ops, mutation{key: generationMarker(source, previous.Generation), value: []byte{1}})
	}
	if e = d.batch(ctx, ops); e != nil {
		return e
	}
	g.published = true
	if e = d.cleanUpload(context.WithoutCancel(ctx), overlay, true); e != nil {
		return e
	}
	if previous.Generation == "" {
		return nil
	}
	return d.cleanGeneration(ctx, generationMarker(source, previous.Generation))
}

func generationMarker(source, id string) string {
	return "g/" + identity(source) + "/" + id
}

// Only invoked for a superseded checkpoint or during exclusive startup.
func (d *DB) cleanGeneration(ctx context.Context, marker string) error {
	prefix := "f/" + strings.TrimPrefix(marker, "g/") + "/"
	err := d.kv.Scan(ctx, prefix, prefix+"\xff", func(k string, ref []byte) error {
		b, e := d.kv.Get(ctx, string(ref))
		if e != nil {
			return e
		}
		ops := []mutation{{key: k, remove: true}, {key: string(ref), remove: true}}
		if len(b) > 0 {
			c, e := decodeCapture(string(ref), b)
			if e != nil {
				return e
			}
			if c.GetRid() != "" {
				ops = append(ops, mutation{key: recordKey(c), remove: true})
			}
		}
		return d.batch(ctx, ops)
	})
	if err != nil {
		return err
	}
	return d.kv.Delete(ctx, marker)
}

func (d *DB) visible(ctx context.Context, c *index.Capture) (bool, error) {
	if strings.HasPrefix(c.GetGeneration(), "upload-") {
		b, e := d.kv.Get(ctx, "u/"+c.GetGeneration())
		return len(b) > 0, e
	}
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

func (d *DB) supersedeImports(ctx context.Context, g *generation, overlay string) (int64, error) {
	var count int64
	var ops []mutation
	flush := func() error {
		if len(ops) == 0 {
			return nil
		}
		e := d.batch(ctx, ops)
		ops = nil
		return e
	}
	prefix := "f/" + identity(g.source) + "/" + g.id + "/"
	err := d.kv.Scan(ctx, prefix, prefix+"\xff", func(_ string, ref []byte) error {
		b, e := d.kv.Get(ctx, string(ref))
		if e != nil {
			return e
		}
		c, e := decodeCapture(string(ref), b)
		if e != nil {
			return e
		}
		c.Sts = timestamppb.New(c.GetSts().AsTime().Truncate(time.Second))
		c.Source = ""
		c.Generation = ""
		base := captureBase(c)
		b, e = d.kv.Get(ctx, base)
		if e != nil || len(b) == 0 {
			return e
		}
		c, e = decodeCapture(base, b)
		if e != nil {
			return e
		}
		c.Generation = overlay
		c.Srt = deletedCapture
		b, e = storagecodec.Encode(c)
		if e != nil {
			return e
		}
		count++
		ops = append(ops, mutation{key: base + overlay, value: b}, mutation{key: "j/" + overlay + "/" + base, value: []byte(base)})
		if len(ops) >= 256 || batchBytes(ops) >= 1<<20 {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	return count, err
}
