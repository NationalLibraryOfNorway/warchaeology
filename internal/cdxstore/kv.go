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
	"uuid"

	"github.com/dgraph-io/badger/v4"
)

const badgerFormat = "badger"

type kv interface {
	batch(context.Context, []mutation) error
	Get(context.Context, string) ([]byte, error)
	Put(context.Context, string, []byte) error
	Delete(context.Context, string) error
	Scan(context.Context, string, string, func(string, []byte) error) error
	Close() error
}
type Options struct {
	Format, Dir string
	ReadOnly    bool
	IndexOnly   bool
	Scratch     bool
}

func openKV(ctx context.Context, o Options) (kv, error) {
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
		return &badgerKV{db: db, lock: lock}, nil
	default:
		return nil, fmt.Errorf("unknown database format %q", o.Format)
	}
}

type badgerKV struct {
	db   *badger.DB
	lock *os.File
}

func (b *badgerKV) Get(ctx context.Context, k string) (v []byte, e error) {
	if e = ctx.Err(); e != nil {
		return
	}
	e = b.db.View(func(tx *badger.Txn) error {
		item, e := tx.Get([]byte(k))
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
	if e := ctx.Err(); e != nil {
		return e
	}
	return b.db.Update(func(tx *badger.Txn) error { return tx.Set([]byte(k), v) })
}

func (b *badgerKV) Delete(ctx context.Context, k string) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	return b.db.Update(func(tx *badger.Txn) error { return tx.Delete([]byte(k)) })
}

func (b *badgerKV) Scan(ctx context.Context, lo, hi string, fn func(string, []byte) error) error {
	return b.db.View(func(tx *badger.Txn) error {
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
	return b.db.Update(func(tx *badger.Txn) error {
		for _, e := range entries {
			var err error
			if e.remove {
				err = tx.Delete([]byte(e.key))
			} else {
				err = tx.Set([]byte(e.key), e.value)
			}
			if err != nil {
				return err
			}
		}
		return ctx.Err()
	})
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
