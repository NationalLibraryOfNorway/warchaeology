// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"uuid"

	"github.com/dgraph-io/badger/v4"
)

const badgerFormat = "badger"

type kv interface {
	catalogueFormat() string
	batch(context.Context, []mutation) error
	Get(context.Context, string) ([]byte, error)
	Put(context.Context, string, []byte) error
	Delete(context.Context, string) error
	Scan(context.Context, string, string, func(string, []byte) error) error
	Close() error
}
type Options struct {
	Format, Dir, TempDir string
	ReadOnly             bool
	IndexOnly            bool
	Scratch              bool
}

func openKV(ctx context.Context, o Options) (*badgerKV, error) {
	if o.Format == badgerFormat {
		for _, legacy := range []string{"cdx-index", "id-index"} {
			if _, e := os.Stat(filepath.Join(o.Dir, legacy)); e == nil {
				return nil, fmt.Errorf("legacy index database; reindex into a new directory")
			}
		}
	}
	switch o.Format {
	case badgerFormat:
		opts := badger.DefaultOptions(o.Dir).WithReadOnly(o.ReadOnly).WithSyncWrites(!o.Scratch).WithMemTableSize(32 << 20).WithNumMemtables(2).WithBlockCacheSize(16 << 20).WithIndexCacheSize(8 << 20).WithValueLogFileSize(64 << 20).WithLogger(nil)
		var lock *os.File
		if runtime.GOOS == "windows" && o.ReadOnly {
			// Badger writers exclusively open LOCK with delete-on-close. A shared
			// read handle excludes writers while permitting concurrent readers.
			var err error
			lock, err = os.OpenFile(filepath.Join(o.Dir, "LOCK"), os.O_RDONLY|os.O_CREATE, 0600)
			if err != nil {
				return nil, err
			}
			opts = opts.WithBypassLockGuard(true)
		}
		db, e := badger.Open(opts)
		if e != nil {
			if lock != nil {
				e = errors.Join(e, lock.Close())
			}
			return nil, e
		}
		return &badgerKV{db: db, lock: lock, tempDir: o.TempDir, versioned: !o.Scratch}, nil
	default:
		return nil, fmt.Errorf("unknown database format %q", o.Format)
	}
}

type badgerKV struct {
	tempDir        string
	versioned      bool
	overflowActive atomic.Bool
	overflowMu     sync.Mutex // serialize last-row removal against concurrent inserts for the same URL
	db             *badger.DB
	lock           *os.File
}

func (b *badgerKV) catalogueFormat() string {
	if b.overflowActive.Load() {
		return overflowFormat
	}
	return formatVersion
}

func (b *badgerKV) Get(ctx context.Context, k string) (v []byte, e error) {
	if e = ctx.Err(); e != nil {
		return
	}
	e = b.db.View(func(tx *badger.Txn) error {
		var item *badger.Item
		var e error
		if ns, ok := captureNamespace(k); ok {
			item, e = lookupCaptureItem(tx, ns, k)
		} else {
			item, e = tx.Get([]byte(k))
		}
		if errors.Is(e, badger.ErrKeyNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		v, e = item.ValueCopy(nil)
		return e
	})
	return
}

func (b *badgerKV) Put(ctx context.Context, k string, v []byte) error {
	return b.batch(ctx, []mutation{{key: k, value: v}})
}

func (b *badgerKV) Delete(ctx context.Context, k string) error {
	return b.batch(ctx, []mutation{{key: k, remove: true}})
}

func (b *badgerKV) Scan(ctx context.Context, lo, hi string, fn func(string, []byte) error) error {
	return b.db.View(func(tx *badger.Txn) error {
		if ns, ok := captureNamespace(lo); ok && hi != "" && hi <= ns+"c0" {
			return scanCaptures(ctx, tx, ns, lo, hi, b.tempDir, fn)
		}
		it := tx.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek([]byte(lo)); it.Valid(); it.Next() {
			if e := ctx.Err(); e != nil {
				return e
			}
			item := it.Item()
			key := string(item.Key())
			if hi != "" && key >= hi {
				break
			}
			v, e := item.ValueCopy(nil)
			if e != nil {
				return e
			}
			if e = fn(key, v); e != nil {
				return e
			}
		}
		return nil
	})
}

func (b *badgerKV) Close() error {
	err := b.db.Close()
	if b.lock != nil {
		err = errors.Join(err, b.lock.Close())
	}
	return err
}

type mutation struct {
	key    string
	value  []byte
	remove bool
}

func (b *badgerKV) batch(ctx context.Context, entries []mutation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	locked := false
	activate := false
	for _, op := range entries {
		if len(op.key) <= InlineURLKeyLimit {
			continue
		}
		ns, ok := captureNamespace(op.key)
		if !ok {
			continue
		}
		url, _, _ := strings.Cut(op.key[len(ns)+2:], " ")
		if len(url) <= InlineURLKeyLimit {
			continue
		}
		if !locked {
			if err := lockContext(ctx, &b.overflowMu); err != nil {
				return err
			}
			defer b.overflowMu.Unlock()
			locked = true
		}
		if !op.remove && b.versioned && !b.overflowActive.Load() {
			activate = true
			break
		}
	}
	for {
		err := b.db.Update(func(tx *badger.Txn) error {
			for _, op := range entries {
				var err error
				if ns, ok := captureNamespace(op.key); ok {
					err = mutateCapture(tx, ns, op)
				} else {
					err = writeMutation(tx, op)
				}
				if err != nil {
					return err
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if activate {
				return tx.Set([]byte("!format"), []byte(overflowFormat))
			}
			return nil
		})
		if err == nil && activate {
			b.overflowActive.Store(true)
		}
		if !errors.Is(err, badger.ErrConflict) {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
func (d *DB) batch(ctx context.Context, entries []mutation) error {
	// The revision is a durable, coalescing notification for derived indexes.
	// It shares the transaction with publication and deletion markers.
	for _, op := range entries {
		if strings.HasPrefix(op.key, "c/") || strings.HasPrefix(op.key, "s/") || strings.HasPrefix(op.key, "u/") || strings.HasPrefix(op.key, "package/") {
			entries = append(entries, mutation{key: "text-revision", value: []byte(uuid.NewV7().String())})
			break
		}
	}
	return d.kv.batch(ctx, entries)
}

// TextRevision changes whenever capture visibility or package metadata changes.
func (d *DB) TextRevision(ctx context.Context) (string, error) {
	b, e := d.kv.Get(ctx, "text-revision")
	return string(b), e
}
