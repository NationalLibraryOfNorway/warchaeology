package cdxstore

import (
	"context"

	"github.com/dgraph-io/badger/v4"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
)

// ScanOrdered seeks directly to an exact URL's last capture or both sides of
// the closest timestamp. Two bounded cursors replace full-history sorting.
//
//nolint:gocyclo // Two directional cursors group visible versions before merging by distance.
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
		makeIterator := func(reverse bool, seek string) *badger.Iterator {
			o := badger.DefaultIteratorOptions
			o.Reverse = reverse
			it := tx.NewIterator(o)
			it.Seek([]byte(seek))
			return it
		}
		next := func(it *badger.Iterator, reverse bool) (*cdx.Capture, error) {
			var chosen *cdx.Capture
			var base string
			for it.Valid() {
				if e := ctx.Err(); e != nil {
					return nil, e
				}
				k := string(it.Item().Key())
				if k < lo || k >= hi {
					if reverse && k >= hi {
						it.Next()
						continue
					}
					break
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
		target, e := timestamp.Parse(q.Closest)
		if e != nil {
			return e
		}
		pivot := "c/" + q.Key + " " + target.UTC().Format("20060102150405")
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
