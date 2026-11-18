package index

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dgraph-io/badger/v4"
	"github.com/nationallibraryofnorway/gowarc/v3"
)

type DigestIndex struct {
	mu        sync.Mutex
	dir       string
	db        *badger.DB
	keepIndex bool
}

func NewDigestIndex(indexDir string, keepIndex bool, newIndex bool) (*DigestIndex, error) {
	dir := filepath.Join(indexDir, "digest-index")

	db, err := badger.Open(badger.DefaultOptions(dir).WithLoggingLevel(badger.WARNING).WithSyncWrites(true))
	if err != nil {
		if strings.Contains(err.Error(), "manifest has unsupported version") {
			return nil, fmt.Errorf("incompatible donor index; reprocess with fresh donor and processed-file indexes and a fresh output directory: %w", err)
		}
		return nil, err
	}

	idx := &DigestIndex{
		db:        db,
		dir:       dir,
		keepIndex: keepIndex,
	}

	if newIndex {
		if err = idx.db.DropAll(); err != nil {
			idx.Close()
			return nil, err
		}
	}

	if err = db.Update(func(tx *badger.Txn) error {
		item, e := tx.Get([]byte("!format"))
		if e == nil {
			return item.Value(func(b []byte) error {
				if string(b) != "dedup-v2" {
					return fmt.Errorf("incompatible donor index; rebuild in a fresh directory")
				}
				return nil
			})
		}
		if e != badger.ErrKeyNotFound {
			return e
		}
		it := tx.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		it.Rewind()
		if it.Valid() {
			return fmt.Errorf("legacy donor index; rebuild in a fresh directory")
		}
		return tx.Set([]byte("!format"), []byte("dedup-v2"))
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = db.View(func(tx *badger.Txn) error {
		_, e := tx.Get([]byte("!pending-output"))
		if e == badger.ErrKeyNotFound {
			return nil
		}
		if e != nil {
			return e
		}
		return fmt.Errorf("unfinished dedup output; use a fresh index and output directory")
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return idx, nil
}

func (digestIndex *DigestIndex) GetDir() string {
	return digestIndex.dir
}

func (digestIndex *DigestIndex) IsRevisit(key string, revisitRef *gowarc.RevisitRef) (*gowarc.RevisitRef, error) {
	var revisitReference *gowarc.RevisitRef
	val, err := marshalDonor(revisitRef)
	if err != nil {
		return nil, err
	}
	err = runWithConflictRetry(func() error {
		return digestIndex.db.Update(func(txn *badger.Txn) error {
			item, err := txn.Get(digestKey(key))
			if err == badger.ErrKeyNotFound {
				return txn.Set(digestKey(key), val)
			}
			if err != nil {
				return err
			}
			return item.Value(func(val []byte) error {
				rr, err := unmarshalDonor(val)
				revisitReference = rr
				return err
			})
		})
	})
	return revisitReference, err
}

func (idx *DigestIndex) Close() error {
	var err error
	if idx.db != nil {
		err = errors.Join(err, idx.db.Close())
		idx.db = nil
	}
	if !idx.keepIndex && idx.dir != "" {
		err = errors.Join(err, os.RemoveAll(idx.dir))
		idx.dir = ""
	}
	return err
}
