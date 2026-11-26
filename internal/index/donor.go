// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"fmt"
	"time"
	"uuid"

	"github.com/dgraph-io/badger/v4"
	"github.com/nationallibraryofnorway/gowarc/v4"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/storagecodec"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func marshalDonor(r *gowarc.RevisitRef) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("missing donor")
	}
	t, e := time.Parse(time.RFC3339Nano, r.TargetDate)
	if e != nil {
		return nil, e
	}
	return storagecodec.Encode(&cdx.Capture{Rid: r.TargetRecordId, Uri: r.TargetUri, Mct: r.Profile, Sts: timestamppb.New(t)})
}
func unmarshalDonor(b []byte) (*gowarc.RevisitRef, error) {
	c, e := storagecodec.Decode(b, "")
	if e != nil {
		return nil, e
	}
	return &gowarc.RevisitRef{
		TargetRecordId: c.GetRid(),
		TargetUri:      c.GetUri(),
		Profile:        c.GetMct(),
		TargetDate:     timestamp.FormatRFC3339(c.GetSts().AsTime()),
	}, nil
}
func digestKey(key string) []byte { return append([]byte("d/"), storagecodec.Digest(key)...) }

// WriteWithDonor serializes lookup, output writing and registration. Failed
// writes never register a donor. New donors remain provisional across restart
// until CommitOutputRun is called after all writers publish successfully.
func (idx *DigestIndex) WriteWithDonor(key string, ref *gowarc.RevisitRef, write func(*gowarc.RevisitRef) error) error {
	return idx.writeDonor("", key, ref, write)
}

type FileDonors struct {
	idx    *DigestIndex
	prefix string
}

func (idx *DigestIndex) NewFileDonors() *FileDonors {
	return &FileDonors{idx: idx, prefix: "staged/" + uuid.NewV7().String() + "/"}
}
func (f *FileDonors) WriteWithDonor(key string, ref *gowarc.RevisitRef, write func(*gowarc.RevisitRef) error) error {
	return f.idx.writeDonor(f.prefix, key, ref, write)
}

// Finish promotes donors only after their file is durable. Other workers may have
// published the same digest meanwhile, in which case the earlier donor wins.
func (f *FileDonors) Finish(publish bool) error {
	f.idx.mu.Lock()
	defer f.idx.mu.Unlock()
	prefix := []byte(f.prefix)
	for {
		n := 0
		err := f.idx.db.Update(func(tx *badger.Txn) error {
			it := tx.NewIterator(badger.DefaultIteratorOptions)
			defer it.Close()
			for it.Seek(prefix); it.ValidForPrefix(prefix) && n < 128; it.Next() {
				item := it.Item()
				key := item.KeyCopy(nil)
				dest := key[len(prefix):]
				if publish {
					_, err := tx.Get(dest)
					if err == badger.ErrKeyNotFound {
						v, e := item.ValueCopy(nil)
						if e != nil {
							return e
						}
						err = tx.Set(dest, v)
					}
					if err != nil {
						return err
					}
				}
				if err := tx.Delete(key); err != nil {
					return err
				}
				n++
			}
			return nil
		})
		if err != nil || n == 0 {
			return err
		}
	}
}
func (idx *DigestIndex) writeDonor(prefix, key string, ref *gowarc.RevisitRef, write func(*gowarc.RevisitRef) error) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	encoded, e := marshalDonor(ref)
	if e != nil {
		return e
	}
	var existing *gowarc.RevisitRef
	e = idx.db.View(func(tx *badger.Txn) error {
		item, e := tx.Get(digestKey(key))
		if e == badger.ErrKeyNotFound && prefix != "" {
			item, e = tx.Get(append([]byte(prefix), digestKey(key)...))
		}
		if e == badger.ErrKeyNotFound {
			return nil
		}
		if e != nil {
			return e
		}
		return item.Value(func(b []byte) error { existing, e = unmarshalDonor(b); return e })
	})
	if e != nil {
		return e
	}
	if e = write(existing); e != nil {
		return e
	}
	if existing != nil {
		return nil
	}
	return idx.db.Update(func(tx *badger.Txn) error { return tx.Set(append([]byte(prefix), digestKey(key)...), encoded) })
}
func (idx *DigestIndex) BeginOutputRun() error {
	return idx.db.Update(func(tx *badger.Txn) error {
		if _, e := tx.Get([]byte("!pending-output")); e == nil {
			return fmt.Errorf("unfinished dedup output; use a fresh index and reprocess inputs into a fresh output directory")
		} else if e != badger.ErrKeyNotFound {
			return e
		}
		return tx.Set([]byte("!pending-output"), []byte{1})
	})
}
func (idx *DigestIndex) CommitOutputRun() error {
	return idx.db.Update(func(tx *badger.Txn) error { return tx.Delete([]byte("!pending-output")) })
}
