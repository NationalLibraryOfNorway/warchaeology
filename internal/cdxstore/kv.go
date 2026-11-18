package cdxstore

import (
	"context"

	"errors"
	"fmt"

	"os"
	"path/filepath"

	"github.com/dgraph-io/badger/v4"
)

type kv interface {
	Get(context.Context, string) ([]byte, error)
	Put(context.Context, string, []byte) error
	Delete(context.Context, string) error
	Scan(context.Context, string, string, func(string, []byte) error) error
	Close() error
}
type Options struct {
	Format, Dir string
	ReadOnly    bool
}

func openKV(ctx context.Context, o Options) (kv, error) {
	if o.Format == "badger" {
		for _, legacy := range []string{"cdx-index", "id-index"} {
			if _, e := os.Stat(filepath.Join(o.Dir, legacy)); e == nil {
				return nil, fmt.Errorf("legacy capture database; reindex into a new directory")
			}
		}
	}
	switch o.Format {
	case "badger":
		db, e := badger.Open(badger.DefaultOptions(o.Dir).WithReadOnly(o.ReadOnly).WithSyncWrites(true).WithLogger(nil))
		if e != nil {
			return nil, e
		}
		return &badgerKV{db}, nil
	default:
		return nil, fmt.Errorf("unknown database format %q", o.Format)
	}
}

type badgerKV struct{ db *badger.DB }

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
func (b *badgerKV) Close() error { return b.db.Close() }
