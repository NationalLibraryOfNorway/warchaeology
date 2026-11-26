// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/dgraph-io/badger/v4"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
)

// InlineURLKeyLimit leaves room for the capture identity and staging suffixes.
const InlineURLKeyLimit = 60000
const overflowFormat = formatVersion + ";long-url-keys-v1"

func validFormat(s string) bool { return s == formatVersion || s == overflowFormat }

// Scratch WACZ indexes group their capture keys under a source hash.
func captureNamespace(k string) (string, bool) {
	if strings.HasPrefix(k, "c/") {
		return "", true
	}
	if len(k) >= 67 && k[64:67] == "/c/" {
		return k[:65], true
	}
	return "", false
}

func uploadJournal(id, base string) string {
	if len(base) > InlineURLKeyLimit {
		return "j/" + id + "/hash/" + identity(base)
	}
	return "j/" + id + "/" + base
}

// The dictionary (d/<digest> => URL) is shared by all rows for that URL.
// The ordered capture suffix stays intact under l/<digest>. Other catalogue
// indexes retain logical references, so legacy and overflow rows resolve alike.
func overflowKey(ns, logical string) (url, dict, physical string) {
	url, tail, has := strings.Cut(logical[len(ns)+2:], " ")
	if !has || len(url) <= InlineURLKeyLimit {
		return "", "", ""
	}
	digest := identity(url)
	return url, ns + "d/" + digest, ns + "l/" + digest + " " + tail
}

func checkDictionary(tx *badger.Txn, dict, url string) error {
	item, err := tx.Get([]byte(dict))
	if err != nil {
		return err
	}
	return item.Value(func(value []byte) error {
		if !bytes.Equal(value, []byte(url)) {
			return errors.New("long URL key digest collision")
		}
		return nil
	})
}

func lookupCaptureItem(tx *badger.Txn, ns, key string) (*badger.Item, error) {
	url, dict, physical := overflowKey(ns, key)
	if physical != "" {
		err := checkDictionary(tx, dict, url)
		if err == nil {
			item, err := tx.Get([]byte(physical))
			if !errors.Is(err, badger.ErrKeyNotFound) {
				return item, err
			}
		} else if !errors.Is(err, badger.ErrKeyNotFound) {
			return nil, err
		}
	}
	return tx.Get([]byte(key))
}

func mutateCapture(tx *badger.Txn, ns string, op mutation) error {
	url, dict, physical := overflowKey(ns, op.key)
	if physical == "" {
		return writeMutation(tx, op)
	}
	err := checkDictionary(tx, dict, url)
	missing := errors.Is(err, badger.ErrKeyNotFound)
	if err != nil && !missing {
		return err
	}
	// Older native writers could store some keys beyond our inline threshold.
	if _, err := tx.Get([]byte(op.key)); err == nil {
		if err := tx.Delete([]byte(op.key)); err != nil {
			return err
		}
	} else if !errors.Is(err, badger.ErrKeyNotFound) {
		return err
	}
	op.key = physical
	if op.remove {
		if missing {
			return nil
		}
		if err := writeMutation(tx, op); err != nil {
			return err
		}
		prefix := []byte(strings.SplitN(physical, " ", 2)[0] + " ")
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := tx.NewIterator(opts)
		defer it.Close()
		it.Seek(prefix)
		if !it.ValidForPrefix(prefix) {
			return tx.Delete([]byte(dict))
		}
		return nil
	}
	if missing {
		if err := tx.Set([]byte(dict), []byte(url)); err != nil {
			return err
		}

	}
	return writeMutation(tx, op)
}

func writeMutation(tx *badger.Txn, op mutation) error {
	if op.remove {
		return tx.Delete([]byte(op.key))
	}
	return tx.Set([]byte(op.key), op.value)
}

// scanCaptures emits logical capture keys in byte order from one snapshot.
// Only the dictionary of distinct oversized URLs needs external sorting.
func scanCaptures(ctx context.Context, tx *badger.Txn, ns, lo, hi, tempDir string, emit func(string, []byte) error) (err error) {
	logicalPrefix := ns + "c/"
	url, _, hasSeparator := strings.Cut(strings.TrimPrefix(lo, logicalPrefix), " ")
	exact := hasSeparator && hi <= logicalPrefix+url+"!"
	inlineOpts := badger.DefaultIteratorOptions
	inlineOpts.Prefix = []byte(logicalPrefix)
	if exact {
		inlineOpts.Prefix = []byte(logicalPrefix + url + " ")
		inlineOpts.PrefetchValues = false
	}
	inline := tx.NewIterator(inlineOpts)
	defer inline.Close()
	inline.Seek([]byte(lo))
	drain := func(until string) error {
		for inline.Valid() {
			if err := ctx.Err(); err != nil {
				return err
			}
			item := inline.Item()
			key := string(item.Key())
			if key >= until {
				return nil
			}
			v, e := item.ValueCopy(nil)
			if e != nil {
				return e
			}
			if e = emit(key, v); e != nil {
				return e
			}
			inline.Next()
		}
		return nil
	}
	visit := func(url string) error {
		p := logicalPrefix + url + " "
		physical := ns + "l/" + identity(url) + " "
		start := physical
		if strings.HasPrefix(lo, p) {
			start += lo[len(p):]
		}
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(physical)
		opts.PrefetchValues = !exact
		it := tx.NewIterator(opts)
		defer it.Close()
		for it.Seek([]byte(start)); it.ValidForPrefix([]byte(physical)); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			item := it.Item()
			k := p + string(item.Key()[len(physical):])
			if k >= hi {
				break
			}
			if err := drain(k); err != nil {
				return err
			}
			v, e := item.ValueCopy(nil)
			if e != nil {
				return e
			}
			if e = emit(k, v); e != nil {
				return e
			}
		}
		return nil
	}
	// A bound containing the capture separator identifies one exact URL.
	if exact {
		if len(url) > InlineURLKeyLimit {
			e := checkDictionary(tx, ns+"d/"+identity(url), url)
			if e == nil {
				if e = visit(url); e != nil {
					return e
				}
			} else if !errors.Is(e, badger.ErrKeyNotFound) {
				return e
			}
		}
		return drain(hi)
	}
	sorted := cdx.NewSorter(tempDir, false)
	defer func() { err = errors.Join(err, sorted.Close()) }()
	prefix := []byte(ns + "d/")
	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	it := tx.NewIterator(opts)
	defer it.Close()
	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := it.Item().Value(func(v []byte) error {
			url := string(v)
			if logicalPrefix+url+"!" <= lo || logicalPrefix+url+" " >= hi {
				return nil
			}
			return sorted.Add(ctx, url)
		}); err != nil {
			return err
		}
	}
	if err := sorted.ForEach(ctx, visit); err != nil {
		return err
	}
	return drain(hi)
}
