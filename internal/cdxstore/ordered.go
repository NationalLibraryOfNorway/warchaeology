// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"errors"
	"strings"

	"github.com/dgraph-io/badger/v4"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
)

// exactCursor merges legacy inline rows with overflow rows for the same URL.
type exactCursor struct {
	inline, overflow          *badger.Iterator
	logical, physical, lo, hi string
	reverse                   bool
}

func (c *exactCursor) current() (*badger.Iterator, string) {
	var chosen *badger.Iterator
	var key string
	if c.inline.Valid() {
		k := string(c.inline.Item().Key())
		if k >= c.lo && k < c.hi {
			chosen, key = c.inline, k
		}
	}
	if c.overflow != nil && c.overflow.ValidForPrefix([]byte(c.physical+" ")) {
		k := c.logical + string(c.overflow.Item().Key()[len(c.physical):])
		if k >= c.lo && k < c.hi && (chosen == nil || (!c.reverse && k < key) || (c.reverse && k > key)) {
			return c.overflow, k
		}
	}
	return chosen, key
}

func (c *exactCursor) Close() {
	c.inline.Close()
	if c.overflow != nil {
		c.overflow.Close()
	}
}

// ScanOrdered seeks directly to an exact URL's last capture or both sides of
// the closest timestamp. Two bounded cursors replace full-history sorting.
//
// Two directional cursors group visible versions before merging by distance.
func (d *DB) ScanOrdered(ctx context.Context, q cdx.Query, emit func(*cdx.Capture) error) error {
	ctx, done, e := d.operation(ctx)
	if e != nil {
		return e
	}
	defer done()
	d.visibility.RLock()
	defer d.visibility.RUnlock()
	return d.kv.(*badgerKV).db.View(func(tx *badger.Txn) error {
		lo, hi := "c/"+q.Lower, "c/"+q.Upper
		logical := "c/" + q.Key
		physical := ""
		if len(q.Key) > InlineURLKeyLimit {
			digest := identity(q.Key)
			e := checkDictionary(tx, "d/"+digest, q.Key)
			if e == nil {
				physical = "l/" + digest
			} else if !errors.Is(e, badger.ErrKeyNotFound) {
				return e
			}
		}
		makeIterator := func(reverse bool, seek string) *exactCursor {
			opts := badger.DefaultIteratorOptions
			opts.Reverse = reverse
			opts.PrefetchValues = false
			openAt := func(prefix, key, upper string) *badger.Iterator {
				opts.Prefix = []byte(prefix)
				it := tx.NewIterator(opts)
				it.Seek([]byte(key))
				if reverse && it.Valid() && string(it.Item().Key()) == upper {
					it.Next()
				}
				return it
			}
			cursor := &exactCursor{inline: openAt("c/", seek, hi), logical: logical, physical: physical, lo: lo, hi: hi, reverse: reverse}
			if physical != "" {
				cursor.overflow = openAt("l/", physical+strings.TrimPrefix(seek, logical), physical+strings.TrimPrefix(hi, logical))
			}
			return cursor
		}
		next := func(cursor *exactCursor, reverse bool) (*cdx.Capture, error) {
			var chosen *cdx.Capture
			var base string
			for {
				it, k := cursor.current()
				if it == nil {
					break
				}
				if e := ctx.Err(); e != nil {
					return nil, e
				}
				b, e := it.Item().ValueCopy(nil)
				if e != nil {
					return nil, e
				}
				c, e := decodeCapture(k, b)
				if e != nil {
					return nil, e
				}
				group := captureBase(c)
				if base != "" && group != base {
					if chosen != nil && chosen.GetSrt() != deletedCapture {
						return chosen, nil
					}
					chosen = nil
				}
				base = group
				ok, e := d.visible(ctx, c)
				if e != nil {
					return nil, e
				}
				if ok && (!reverse || chosen == nil) {
					chosen = c
				}
				it.Next()
			}
			if chosen != nil && chosen.GetSrt() != deletedCapture {
				return chosen, nil
			}
			return nil, nil
		}
		if q.Order == "reverse" {
			it := makeIterator(true, hi)
			defer it.Close()
			for {
				c, e := next(it, true)
				if e != nil {
					return e
				}
				if c == nil {
					return nil
				}
				if e = emit(c); e != nil {
					return e
				}
			}
		}
		target, e := timestamp.ParsePartial(q.Closest)
		if e != nil {
			return e
		}
		pivot := "c/" + q.Key + " " + timestamp.Format14(target)
		left, right := makeIterator(true, pivot), makeIterator(false, pivot)
		defer left.Close()
		defer right.Close()
		a, e := next(left, true)
		if e != nil {
			return e
		}
		b, e := next(right, false)
		if e != nil {
			return e
		}
		leftIsCloser := func() bool {
			ls, ln := timestamp.Distance(a.GetSts().AsTime(), target)
			rs, rn := timestamp.Distance(b.GetSts().AsTime(), target)
			return ls < rs || (ls == rs && ln <= rn)
		}
		for a != nil || b != nil {
			if a != nil && (b == nil || leftIsCloser()) {
				if e = emit(a); e != nil {
					return e
				}
				a, e = next(left, true)
			} else {
				if e = emit(b); e != nil {
					return e
				}
				b, e = next(right, false)
			}
			if e != nil {
				return e
			}
		}
		return nil
	})
}
