package index

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/dgraph-io/badger/v4"
	"github.com/nationallibraryofnorway/gowarc/v3"
)

type DigestIndex struct {
	dir       string
	db        *badger.DB
	keepIndex bool
}

func NewDigestIndex(indexDir string, keepIndex bool, newIndex bool) (*DigestIndex, error) {
	dir := filepath.Join(indexDir, "digest-index")

	db, err := badger.Open(badger.DefaultOptions(dir).WithLoggingLevel(badger.WARNING))
	if err != nil {
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

	return idx, nil
}

func (digestIndex *DigestIndex) GetDir() string {
	return digestIndex.dir
}

func (digestIndex *DigestIndex) IsRevisit(key string, revisitRef *gowarc.RevisitRef) (*gowarc.RevisitRef, error) {
	var revisitReference *gowarc.RevisitRef
	val, err := MarshalRevisitRef(revisitRef)
	if err != nil {
		return nil, err
	}
	err = runWithConflictRetry(func() error {
		return digestIndex.db.Update(func(txn *badger.Txn) error {
			item, err := txn.Get([]byte(key))
			if err == badger.ErrKeyNotFound {
				return txn.Set([]byte(key), val)
			}
			if err != nil {
				return err
			}
			return item.Value(func(val []byte) error {
				rr, err := UnmarshalRevisitRef(val)
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
