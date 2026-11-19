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
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/storagecodec"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type generationKey struct{}
type checkpoint struct {
	Generation string                  `json:"generation"`
	Count      int64                   `json:"count"`
	File       *schema.FileInfo        `json:"file"`
	CRC        *uint32                 `json:"crc,omitempty"`
	Container  *filewalker.FileVersion `json:"container,omitempty"`
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
	g.ops = append(g.ops, mutation{key: key, value: b}, mutation{key: "f/" + identity(g.source) + "/" + g.id + "/" + identity(key), value: []byte(key)})
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
	return d.FinishFileVersion(ctx, source, filewalker.FileVersion{Size: size, Modified: modified}, nil)
}

func (d *DB) FinishFileVersion(ctx context.Context, source string, version filewalker.FileVersion, container *filewalker.FileVersion) error {
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
	if err := g.flush(ctx, d); err != nil {
		return err
	}
	return d.publishFiles(ctx, []pendingFile{{source: source, generation: g, version: version, container: container}}, nil)
}

type pendingFile struct {
	source     string
	generation *generation
	version    filewalker.FileVersion
	container  *filewalker.FileVersion
}

// publishFiles switches all checkpoints and package metadata in one transaction.
func (d *DB) publishFiles(ctx context.Context, files []pendingFile, packages []PackageInfo) error {
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	d.visibility.Lock()
	defer d.visibility.Unlock()
	if err := d.recoverUploads(ctx); err != nil {
		return err
	}
	ops, removed, err := d.packageMutations(ctx, packages)
	if err != nil {
		return err
	}
	for source := range removed {
		files = append(files, pendingFile{source: source})
	}
	if len(files) == 0 {
		return d.batch(ctx, ops)
	}
	count, err := d.count(ctx)
	if err != nil {
		return err
	}
	overlay := "upload-native-" + uuid.NewV7().String()
	published := false
	defer func() {
		if !published {
			_ = d.cleanUpload(context.WithoutCancel(ctx), overlay, false)
		}
	}()
	var obsolete []string
	for _, file := range files {
		source, g := file.source, file.generation
		key := "s/" + identity(source)
		raw, err := d.kv.Get(ctx, key)
		if err != nil {
			return err
		}
		if len(raw) > 0 {
			var previous checkpoint
			if err := json.Unmarshal(raw, &previous); err != nil {
				return err
			}
			previousCount, err := d.countGeneration(ctx, source, previous)
			if err != nil {
				return err
			}
			count -= previousCount
			marker := generationMarker(source, previous.Generation)
			obsolete = append(obsolete, marker)
			ops = append(ops, mutation{key: marker, value: []byte{1}})
		}
		if g == nil {
			ops = append(ops, mutation{key: key, remove: true})
			continue
		}
		cp := checkpoint{
			Generation: g.id,
			Count:      g.count,
			CRC:        file.version.CRC,
			Container:  file.container,
			File: &schema.FileInfo{
				Path:         source,
				Size:         file.version.Size,
				LastModified: timestamppb.New(file.version.Modified),
			},
		}
		if !d.indexOnly {
			cp.File.Name = sourceBasename(source)
		}
		b, err := json.Marshal(cp)
		if err != nil {
			return err
		}
		replaced, err := d.supersedeImports(ctx, g, overlay)
		if err != nil {
			return err
		}
		count += g.count - replaced
		ops = append(ops, mutation{key: generationMarker(source, g.id), remove: true}, mutation{key: key, value: b})
	}
	ops = append(ops, mutation{key: countKey, value: []byte(strconv.FormatInt(count, 10))}, mutation{key: "u/" + overlay, value: []byte{1}})
	if err := d.batch(ctx, ops); err != nil {
		return err
	}
	published = true
	for _, file := range files {
		if file.generation != nil {
			file.generation.published = true
		}
	}
	// The committed state is visible even if cleanup fails. Startup recovers
	// the upload journal and obsolete generations.
	ctx = context.WithoutCancel(ctx)
	if err := d.cleanUpload(ctx, overlay, true); err != nil {
		return err
	}
	for _, marker := range obsolete {
		if err := d.cleanGeneration(ctx, marker); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) countGeneration(ctx context.Context, source string, previous checkpoint) (int64, error) {
	prefix := "f/" + identity(source) + "/" + previous.Generation + "/"
	var entries, count int64
	var legacy, modern bool
	err := d.kv.Scan(ctx, prefix, prefix+"\xff", func(key string, ref []byte) error {
		entries++
		legacy = legacy || len(key)-len(prefix) == 20
		modern = modern || len(key)-len(prefix) != 20
		b, err := d.kv.Get(ctx, string(ref))
		if len(b) > 0 {
			count++
		}
		return err
	})
	if err != nil || (entries >= previous.Count && (!legacy || !modern)) {
		return count, err
	}
	// Older journals used an offset key and could lose entries for aliases
	// of the same record. Recover their references before retiring the generation.
	count = 0
	err = d.kv.Scan(ctx, "c/", "c0", func(key string, value []byte) error {
		if !strings.HasSuffix(key, " "+previous.Generation) {
			return nil
		}
		c, err := decodeCapture(key, value)
		if err != nil || c.GetSource() != source {
			return err
		}
		count++
		// Replace the old journal separately below, so duplicate references
		// cannot inflate counts if the operation is retried.
		return d.kv.Put(ctx, prefix+identity(key), []byte(key))
	})
	if err != nil {
		return 0, err
	}
	err = d.kv.Scan(ctx, prefix, prefix+"\xff", func(key string, _ []byte) error {
		if len(key)-len(prefix) == 20 {
			return d.kv.Delete(ctx, key)
		}
		return nil
	})
	return count, err
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
	pending := make(map[string]bool)
	flush := func() error {
		if len(ops) == 0 {
			return nil
		}
		e := d.batch(ctx, ops)
		ops = nil
		clear(pending)
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
		journal := "j/" + overlay + "/" + base
		if pending[journal] {
			return nil
		}
		written, e := d.kv.Get(ctx, journal)
		if e != nil || len(written) > 0 {
			return e
		}
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
		pending[journal] = true
		ops = append(ops, mutation{key: base + overlay, value: b}, mutation{key: journal, value: []byte(base)})
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
