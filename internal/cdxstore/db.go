// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"uuid"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/storagecodec"
)

const rebuildInstructions = "rebuild native indexes from archives or reimport uploaded collections from CDXJ into a new database"

const countKey = "!count"

const formatVersion = "warchaeology-captures-v4;codec-1;whatwg-url-surt-v0.9.1"

var ErrReadOnly = errors.New("database is read-only")

type DB struct {
	closing    bool
	indexOnly  bool
	dir        string
	mutationMu sync.Mutex
	deleted    map[string]bool
	visibility sync.RWMutex
	kv         kv
	readOnly   bool
	mu         sync.Mutex
	tasks      map[string]context.CancelFunc
	wg         sync.WaitGroup
}

func Open(ctx context.Context, o Options) (*DB, error) {
	k, e := openKV(ctx, o)
	if e != nil {
		return nil, e
	}
	fail := func(e error) (*DB, error) { return nil, errors.Join(e, k.Close()) }
	v, e := k.Get(ctx, "!format")
	if e != nil {
		return fail(e)
	}
	if len(v) == 0 {
		nonempty := false
		if e = k.Scan(ctx, "", "", func(_ string, _ []byte) error { nonempty = true; return errStop }); e != nil && !errors.Is(e, errStop) {
			return fail(e)
		}
		if nonempty || o.ReadOnly {
			return fail(fmt.Errorf("unversioned index database: %s", rebuildInstructions))
		}
		if e = k.Put(ctx, "!format", []byte(formatVersion)); e != nil {
			return fail(e)
		}
	} else if string(v) != formatVersion {
		return fail(fmt.Errorf("incompatible index database: %s", rebuildInstructions))
	}
	mode, e := k.Get(ctx, "!capabilities")
	if e != nil {
		return fail(e)
	}
	if len(mode) == 0 {
		if o.IndexOnly {
			mode = []byte("index-only")
		} else {
			mode = []byte("full")
		}
		if e = k.Put(ctx, "!capabilities", mode); e != nil {
			return fail(e)
		}
	}
	if string(mode) != "index-only" && string(mode) != "full" {
		return fail(fmt.Errorf("invalid database capabilities"))
	}
	d := &DB{
		kv:        k,
		readOnly:  o.ReadOnly,
		indexOnly: string(mode) == "index-only",
		dir:       o.Dir,
		tasks:     map[string]context.CancelFunc{},
		deleted:   map[string]bool{},
	}
	pending, e := k.Get(ctx, resetKey)
	if e != nil {
		return fail(e)
	}
	if len(pending) > 0 {
		if o.ReadOnly {
			return fail(errors.New("interrupted index reset requires writable recovery"))
		}
		if e = d.finishReset(ctx); e != nil {
			return fail(e)
		}
	}
	if !o.ReadOnly {
		if e = d.recoverUploads(ctx); e != nil {
			return fail(e)
		}
		if e = k.Scan(ctx, "g/", "g0", func(marker string, _ []byte) error { return d.cleanGeneration(ctx, marker) }); e != nil {
			return fail(e)
		}
		entries, e := os.ReadDir(o.Dir)
		if e != nil {
			return fail(e)
		}
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), ".upload-") {
				if e = os.RemoveAll(filepath.Join(o.Dir, entry.Name())); e != nil {
					return fail(e)
				}
			}
		}
	}
	return d, nil
}

var errStop = errors.New("stop scan")

func (d *DB) Close() error {
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return nil
	}
	d.closing = true
	for _, c := range d.tasks {
		c()
	}
	d.mu.Unlock()
	d.wg.Wait()
	return d.kv.Close()
}

func (d *DB) Flush(ctx context.Context) error { return ctx.Err() }

func identity(source string) string {
	h := sha256.Sum256([]byte(source))
	return hex.EncodeToString(h[:])
}

func captureIdentityPrefix(c *index.Capture) string {
	return "c/" + c.GetSsu() + " " + c.GetSts().AsTime().UTC().Format("20060102150405.000000000") + " " + identity(c.GetFilename()) + " " + fmt.Sprintf("%020d", c.GetOffset()) + " "
}
func captureBase(c *index.Capture) string {
	return captureIdentityPrefix(c) + identity(c.GetSource()) + " "
}

func captureKey(c *index.Capture) string { return captureBase(c) + c.GetGeneration() }
func recordKey(c *index.Capture) string {
	return "i/" + identity(c.GetRid()) + "/" + identity(c.GetSource()) + "/" + fmt.Sprintf("%020d", c.GetOffset()) + "/" + c.GetGeneration()
}
func decodeCapture(k string, b []byte) (*index.Capture, error) {
	key, _, _ := strings.Cut(strings.TrimPrefix(k, "c/"), " ")
	return storagecodec.Decode(b, key)
}
func (d *DB) WriteCapture(ctx context.Context, c *index.Capture) error {
	if d.readOnly {
		return ErrReadOnly
	}
	if g, ok := ctx.Value(generationKey{}).(*generation); ok {
		return g.write(ctx, d, c)
	}
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	d.visibility.Lock()
	defer d.visibility.Unlock()
	b, e := storagecodec.Encode(c)
	if e != nil {
		return e
	}
	key := captureKey(c)
	old, e := d.kv.Get(ctx, key)
	if e != nil {
		return e
	}
	ops := []mutation{{key: key, value: b}}
	if len(old) == 0 && c.GetGeneration() == "" {
		count, e := d.count(ctx)
		if e != nil {
			return e
		}
		ops = append(ops, mutation{key: countKey, value: []byte(strconv.FormatInt(count+1, 10))})
	}
	if len(old) > 0 {
		previous, e := decodeCapture(key, old)
		if e != nil {
			return e
		}
		if previous.GetRid() != "" && previous.GetRid() != c.GetRid() {
			ops = append(ops, mutation{key: recordKey(previous), remove: true})
		}
	}
	if !d.indexOnly && c.GetRid() != "" {
		ops = append(ops, mutation{key: recordKey(c), value: []byte(captureKey(c))})
	}
	return d.batch(ctx, ops)
}
func (d *DB) Scan(ctx context.Context, lower, upper string, emit func(*index.Capture) error) error {
	ctx, done, e := d.operation(ctx)
	if e != nil {
		return e
	}
	defer done()
	d.visibility.RLock()
	defer d.visibility.RUnlock()
	if upper == "" {
		upper = "\xff"
	}
	var current *index.Capture
	var base string
	flush := func() error {
		if current != nil && current.GetSrt() != deletedCapture {
			return emit(current)
		}
		return nil
	}
	err := d.kv.Scan(ctx, "c/"+lower, "c/"+upper, func(k string, v []byte) error {
		c, e := decodeCapture(k, v)
		if e != nil {
			return e
		}
		next := captureBase(c)
		if next != base {
			if e = flush(); e != nil {
				return e
			}
			current = nil
			base = next
		}
		visible, e := d.visible(ctx, c)
		if e != nil {
			return e
		}
		if visible {
			current = c
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

// CapturesByID visits every occurrence, including duplicate IDs in one file.
func (d *DB) CapturesByID(ctx context.Context, id string, emit func(*index.Capture) error) error {
	ctx, done, e := d.operation(ctx)
	if e != nil {
		return e
	}
	defer done()
	d.visibility.RLock()
	defer d.visibility.RUnlock()
	return d.kv.Scan(ctx, "i/"+identity(id)+"/", "i/"+identity(id)+"0", func(_ string, ref []byte) error {
		b, err := d.kv.Get(ctx, string(ref))
		if err != nil {
			return err
		}
		if len(b) == 0 {
			return nil
		}
		c, err := decodeCapture(string(ref), b)
		if err != nil {
			return err
		}
		visible, err := d.currentIDCapture(ctx, c)
		if err != nil || !visible {
			return err
		}
		return emit(c)
	})
}

// currentIDCapture checks committed overlays while their old ID keys await cleanup.
// The caller holds the visibility read lock.
func (d *DB) currentIDCapture(ctx context.Context, c *index.Capture) (bool, error) {
	ok, err := d.visible(ctx, c)
	if !ok || err != nil {
		return false, err
	}
	prefix := captureBase(c) + "upload-"
	superseded := false
	err = d.kv.Scan(ctx, prefix, prefix+"\xff", func(k string, v []byte) error {
		overlay, err := decodeCapture(k, v)
		if err != nil {
			return err
		}
		committed, err := d.visible(ctx, overlay)
		superseded = superseded || committed
		return err
	})
	return !superseded, err
}

func (d *DB) StorageRefById(ctx context.Context, id string) (ref string, err error) {
	err = d.CapturesByID(ctx, id, func(c *index.Capture) error {
		if ref != "" && ref != c.GetRef() {
			return fmt.Errorf("ambiguous record ID")
		}
		ref = c.GetRef()
		return nil
	})
	return
}

func (d *DB) GetStorageRef(ctx context.Context, id string) (string, error) {
	return d.StorageRefById(ctx, id)
}

func (d *DB) PathByFilename(name string) (string, error) {
	fi, e := d.GetFileInfo(context.Background(), name)
	if e != nil {
		return "", e
	}
	if fi == nil {
		return "", fmt.Errorf("file not indexed")
	}
	return fi.GetPath(), nil
}

func (d *DB) GetFileInfo(ctx context.Context, name string) (*schema.FileInfo, error) {
	return d.sourceInfo(ctx, name)
}

func (d *DB) Delete(ctx context.Context) error {
	if d.readOnly {
		return ErrReadOnly
	}
	if err := d.kv.Put(ctx, resetKey, []byte{1}); err != nil {
		return err
	}
	return d.finishReset(ctx)
}

const resetKey = "!reset"

func (d *DB) finishReset(ctx context.Context) error {
	for {
		var ops []mutation
		bytes := 0
		err := d.kv.Scan(ctx, "", "", func(k string, _ []byte) error {
			switch k {
			case "!format", "!capabilities", resetKey:
				return nil
			}
			ops = append(ops, mutation{key: k, remove: true})
			bytes += len(k)
			if len(ops) >= 256 || bytes >= 1<<20 {
				return errStop
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStop) {
			return err
		}
		if len(ops) == 0 {
			return d.kv.batch(ctx, []mutation{{key: resetKey, remove: true}, {key: "text-revision", value: []byte(uuid.NewV7().String())}})
		}
		if err := d.kv.batch(ctx, ops); err != nil {
			return err
		}
	}
}

func queryFromRequest(req index.Request) index.Query {
	key := req.Ssurt()
	q := index.Query{
		Key:     key,
		Match:   "exact",
		Lower:   key + " ",
		Upper:   key + "!",
		Limit:   req.Limit(),
		Closest: req.Closest(),
	}
	if key == "" {
		q.Lower = ""
		q.Upper = ""
		q.Match = "prefix"
	}
	switch req.MatchType() {
	case index.MatchTypePrefix:
		q.Match = "prefix"
		q.Lower = key
		q.Upper = key + "\xff"
	case index.MatchTypeHost:
		q.Match = "host"
		q.Lower = strings.SplitN(key, ")", 2)[0] + ")"
		q.Upper = q.Lower + "\xff"
	case index.MatchTypeDomain:
		q.Match = "domain"
		q.Lower = strings.SplitN(key, ")", 2)[0]
		q.Upper = q.Lower + "\xff"
	}
	switch req.Sort() {
	case index.SortDesc:
		q.Order = "reverse"
	case index.SortClosest:
		q.Order = "closest"
	}
	return q
}

func (d *DB) IndexOnly() bool { return d.indexOnly }
func (d *DB) Capabilities() map[string]any {
	return map[string]any{
		"format":                   formatVersion,
		"indexOnly":                d.indexOnly,
		"recordIDLookup":           !d.indexOnly,
		"nativeContent":            !d.indexOnly,
		"completeRecordIDCoverage": false,
		"writable":                 !d.readOnly,
	}
}
func (d *DB) Stats(ctx context.Context) (map[string]any, error) {
	n, e := d.count(ctx)
	if e != nil {
		return nil, e
	}
	return map[string]any{"captures": n, "capabilities": d.Capabilities()}, nil
}

func (d *DB) count(ctx context.Context) (int64, error) {
	b, e := d.kv.Get(ctx, countKey)
	if e != nil {
		return 0, e
	}
	if len(b) == 0 {
		return 0, nil
	}
	return strconv.ParseInt(string(b), 10, 64)
}

func (d *DB) Directory() string { return d.dir }

// operation makes shutdown cancel and join synchronous streaming operations too.
func (d *DB) operation(ctx context.Context) (context.Context, func(), error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return nil, nil, fmt.Errorf("database closed")
	}
	ctx, cancel := context.WithCancel(ctx)
	id := "operation-" + uuid.NewV7().String()
	d.tasks[id] = cancel
	d.wg.Add(1)
	return ctx, func() { cancel(); d.DeleteTask(id) }, nil
}
