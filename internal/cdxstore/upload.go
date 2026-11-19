// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"uuid"

	"github.com/dgraph-io/badger/v4"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/storagecodec"
)

const deletedCapture = "!deleted"

type Rejection struct {
	Line  int    `json:"line"`
	Error string `json:"error"`
}
type UploadResult struct {
	Accepted    int         `json:"accepted"`
	Rejected    int         `json:"rejected"`
	Diagnostics []Rejection `json:"diagnostics,omitempty"`
	Report      string      `json:"report,omitempty"`
}
type UploadOptions struct {
	SkipBad bool
	Delete  bool
	MaxLine int
	Reject  func(Rejection) error
}

// Upload stages on disk before publication. A durable marker publishes all
// versions together. Journal references permit bounded, idempotent reclamation.
//
//nolint:gocyclo,maintidx // Validation, staging and publication have distinct cleanup boundaries.
func (d *DB) Upload(ctx context.Context, r io.Reader, o UploadOptions) (result UploadResult, err error) {
	if d.readOnly {
		return result, ErrReadOnly
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return result, err
	}
	defer done()
	if o.MaxLine == 0 {
		o.MaxLine = 1 << 20
	}
	dir, err := os.MkdirTemp(d.dir, ".upload-")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	stage, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(false).WithMemTableSize(16 << 20).WithBlockCacheSize(8 << 20).WithValueLogFileSize(16 << 20))
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, stage.Close()) }()
	id := "upload-" + uuid.NewV7().String()
	reader := bufio.NewReader(r)
	line := 0
	for {
		raw, oversize, readErr := readUploadLine(ctx, reader, o.MaxLine)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return result, readErr
		}
		line++
		var c *cdx.Capture
		var e error
		if oversize {
			e = ErrLineTooLong
		} else {
			c, e = cdx.ParseIngestLine(raw)
		}
		if e != nil {
			rejection := Rejection{line, e.Error()}
			result.Rejected++
			if o.Reject != nil {
				if err = o.Reject(rejection); err != nil {
					return result, err
				}
			}
			if len(result.Diagnostics) < 100 {
				result.Diagnostics = append(result.Diagnostics, rejection)
			}
			if !o.SkipBad {
				return result, fmt.Errorf("line %d: %w", line, e)
			}
			continue
		}
		c.Generation = id
		if o.Delete {
			c.Srt = deletedCapture
		}
		b, e := storagecodec.Encode(c)
		if e != nil {
			return result, e
		}
		key := captureBase(c)
		err = stage.Update(func(tx *badger.Txn) error {
			item, e := tx.Get([]byte(key))
			if e == nil {
				return item.Value(func(previous []byte) error {
					if !bytes.Equal(previous, b) {
						return fmt.Errorf("line %d: conflicting values for capture identity", line)
					}
					return nil
				})
			}
			if !errors.Is(e, badger.ErrKeyNotFound) {
				return e
			}
			return tx.Set([]byte(key), b)
		})
		if err != nil {
			return result, err
		}
		result.Accepted++
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = lockContext(ctx, &d.mutationMu); err != nil {
		return result, err
	}
	defer d.mutationMu.Unlock()
	if err = lockContext(ctx, &d.visibility); err != nil {
		return result, err
	}
	err = d.recoverUploads(ctx)
	d.visibility.Unlock()
	if err != nil {
		return result, err
	}
	published := false
	defer func() {
		if !published {
			_ = d.cleanUpload(context.WithoutCancel(ctx), id, false)
		}
	}()
	delta := int64(0)
	ops := make([]mutation, 0, 256)
	flush := func() error {
		if len(ops) == 0 {
			return nil
		}
		e := d.batch(ctx, ops)
		ops = ops[:0]
		return e
	}
	err = stage.View(func(tx *badger.Txn) error {
		it := tx.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			if e := ctx.Err(); e != nil {
				return e
			}
			base := string(it.Item().Key())
			b, e := it.Item().ValueCopy(nil)
			if e != nil {
				return e
			}
			incoming, e := decodeCapture(base, b)
			if e != nil {
				return e
			}
			prefix := incoming.GetSsu() + " " + incoming.GetSts().AsTime().UTC().Format("20060102150405") + "."
			exists := false
			e = d.Scan(ctx, prefix, prefix+"\xff", func(old *cdx.Capture) error {
				if old.GetOffset() != incoming.GetOffset() || old.GetFilename() != incoming.GetFilename() {
					return nil
				}
				if old.GetSource() == "" && !o.Delete {
					exists = true
					return nil
				}
				old.Generation = id
				old.Srt = deletedCapture
				encoded, e := storagecodec.Encode(old)
				if e != nil {
					return e
				}
				oldBase := captureBase(old)
				ops = append(ops, mutation{key: oldBase + id, value: encoded}, mutation{key: "j/" + id + "/" + oldBase, value: []byte(oldBase)})
				delta--
				if len(ops) >= 256 || batchBytes(ops) >= 1<<20 {
					return flush()
				}
				return nil
			})
			if e != nil {
				return e
			}
			if !o.Delete {
				if !exists {
					delta++
				}
				ops = append(ops, mutation{key: base + id, value: b}, mutation{key: "j/" + id + "/" + base, value: []byte(base)})
			}
			if len(ops) >= 256 || batchBytes(ops) >= 1<<20 {
				if e = flush(); e != nil {
					return e
				}
			}
		}
		return flush()
	})
	if err != nil {
		return result, err
	}
	if err = lockContext(ctx, &d.visibility); err != nil {
		return result, err
	}
	defer d.visibility.Unlock()
	count, err := d.count(ctx)
	if err != nil {
		return result, err
	}
	if err = d.batch(ctx, []mutation{{key: "u/" + id, value: []byte{1}}, {key: countKey, value: []byte(strconv.FormatInt(count+delta, 10))}}); err != nil {
		return result, err
	}
	published = true
	// Cleanup failures do not undo an acknowledged publication. Reopening retries
	// this journal; the committed versions remain queryable until reclaimed.
	_ = d.cleanUpload(context.WithoutCancel(ctx), id, true)
	return result, nil
}

func (d *DB) cleanUpload(ctx context.Context, id string, published bool) error {
	prefix := "j/" + id + "/"
	err := d.kv.Scan(ctx, prefix, prefix+"\xff", func(j string, base []byte) error {
		key := string(base) + id
		b, e := d.kv.Get(ctx, key)
		if e != nil {
			return e
		}
		ops := []mutation{{key: j, remove: true}, {key: key, remove: true}}
		if published {
			if e = d.kv.Scan(ctx, string(base), string(base)+"\xff", func(k string, v []byte) error {
				if k == key {
					return nil
				}
				old, e := decodeCapture(k, v)
				if e != nil {
					return e
				}
				visible, e := d.visible(ctx, old)
				if e != nil || !visible {
					return e
				}
				ops = append(ops, mutation{key: k, remove: true})
				if old.GetRid() != "" {
					ops = append(ops, mutation{key: recordKey(old), remove: true})
				}
				return nil
			}); e != nil {
				return e
			}
		}
		if published && len(b) > 0 {
			c, e := decodeCapture(key, b)
			if e != nil {
				return e
			}
			c.Generation = ""
			if c.GetSrt() == deletedCapture {
				ops = append(ops, mutation{key: string(base), remove: true})
			} else {
				b, e = storagecodec.Encode(c)
				if e != nil {
					return e
				}
				ops = append(ops, mutation{key: string(base), value: b})
			}
		}
		return d.batch(ctx, ops)
	})
	if err != nil {
		return err
	}
	return d.kv.Delete(ctx, "u/"+id)
}
func (d *DB) recoverUploads(ctx context.Context) error {
	// All journal keys for one upload are adjacent. Only one ID is retained.
	previous := ""
	err := d.kv.Scan(ctx, "j/", "j0", func(k string, _ []byte) error {
		id, _, _ := strings.Cut(k[2:], "/")
		if id == previous {
			return nil
		}
		previous = id
		b, e := d.kv.Get(ctx, "u/"+id)
		if e != nil {
			return e
		}
		return d.cleanUpload(ctx, id, len(b) > 0)
	})
	if err != nil {
		return err
	}
	// A crash after removing the final journal entry can leave a marker only.
	return d.kv.Scan(ctx, "u/", "u0", func(k string, _ []byte) error { return d.kv.Delete(ctx, k) })
}
func (d *DB) Export(ctx context.Context, w io.Writer) error {
	return d.Scan(ctx, "", "", func(c *cdx.Capture) error {
		b, e := cdx.Line(c)
		if e != nil {
			return e
		}
		_, e = w.Write(append(b, '\n'))
		return e
	})
}
func (d *DB) Backup(ctx context.Context, w io.Writer) error {
	ctx, done, e := d.operation(ctx)
	if e != nil {
		return e
	}
	defer done()
	d.visibility.RLock()
	defer d.visibility.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := d.kv.(*badgerKV).db.Backup(w, 0)
	return err
}

// Restore requires a new directory and never modifies an existing database.
func Restore(ctx context.Context, dir string, r io.Reader) error {
	return RestoreWithFulltext(ctx, dir, r, "")
}

// RestoreWithFulltext also installs a validated, staged full-text index when supplied.
func RestoreWithFulltext(ctx context.Context, dir string, r io.Reader, textDir string) (err error) {
	dir = filepath.Clean(dir)
	if err = os.MkdirAll(filepath.Dir(dir), 0750); err != nil {
		return err
	}
	if err = os.Mkdir(dir, 0750); errors.Is(err, os.ErrExist) {
		return fmt.Errorf("restore destination must not exist: %w", err)
	} else if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			err = errors.Join(err, os.RemoveAll(dir))
		}
	}()
	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(true))
	if err != nil {
		return err
	}
	err = db.Load(r, 16)
	if err == nil {
		err = db.View(func(tx *badger.Txn) error {
			item, e := tx.Get([]byte("!format"))
			if e != nil {
				return fmt.Errorf("backup lacks format metadata: %w", e)
			}
			return item.Value(func(b []byte) error {
				if string(b) != formatVersion {
					return fmt.Errorf("incompatible backup format")
				}
				return nil
			})
		})
	}
	err = errors.Join(err, db.Close())
	if err != nil {
		return err
	}
	restored, err := Open(ctx, Options{Format: badgerFormat, Dir: dir})
	if err != nil {
		return err
	}
	if textDir != "" {
		err = os.Rename(textDir, filepath.Join(dir, "fulltext"))
	}
	err = errors.Join(err, restored.Close())
	complete = err == nil
	return err
}

func batchBytes(ops []mutation) int {
	n := 0
	for _, op := range ops {
		n += len(op.key) + len(op.value) + 32
	}
	return n
}

// Compact reclaims obsolete versions and value-log space. The CLI opens an
// exclusive offline handle so native indexing cannot compete with maintenance.
func (d *DB) Compact(ctx context.Context) error {
	if d.readOnly {
		return ErrReadOnly
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return err
	}
	defer done()
	d.mutationMu.Lock()
	defer d.mutationMu.Unlock()
	d.visibility.Lock()
	defer d.visibility.Unlock()
	if err = d.recoverUploads(ctx); err != nil {
		return err
	}
	db := d.kv.(*badgerKV).db
	if err = db.Flatten(2); err != nil {
		return err
	}
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		err = db.RunValueLogGC(.5)
		if errors.Is(err, badger.ErrNoRewrite) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

var ErrLineTooLong = errors.New("CDXJ line exceeds size limit")

func readUploadLine(ctx context.Context, r *bufio.Reader, maximum int) ([]byte, bool, error) {
	var line []byte
	oversize := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		fragment, err := r.ReadSlice('\n')
		if !oversize {
			if len(line)+len(fragment) > maximum+2 {
				oversize = true
				line = nil
			} else {
				line = append(line, fragment...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(fragment) == 0 && len(line) == 0 && !oversize {
			return nil, false, io.EOF
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		return line, oversize || len(line) > maximum, nil
	}
}
