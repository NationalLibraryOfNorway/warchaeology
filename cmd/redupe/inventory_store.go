// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package redupe

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/storagecodec"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
)

// The occurrence inventory is stored once. All lookup indexes contain only
// references into x/record; the separate ledger contains publication evidence.
type inventoryCatalog struct{ o *RedupeOptions }

func (c *inventoryCatalog) Close() error { return nil }
func (c *inventoryCatalog) BeginFile(ctx context.Context, _ string) (context.Context, error) {
	return ctx, ctx.Err()
}
func (c *inventoryCatalog) FinishFile(ctx context.Context, _ string, _ int64, _ time.Time) error {
	return ctx.Err()
}
func (c *inventoryCatalog) WriteCapture(ctx context.Context, v *cdx.Capture) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ref := v.GetRef()
	key := "x/lookup-url/" + v.GetSsu() + " " + timestamp.FormatNano(v.GetSts().AsTime()) + " " + hashString(ref)
	return c.o.scratch.Update(func(tx *badger.Txn) error {
		if err := tx.Set([]byte(key), []byte(hashString(ref))); err != nil {
			return err
		}
		return tx.Set([]byte("x/lookup-id/"+hashString(v.GetRid())+"/"+hashString(ref)), []byte(hashString(ref)))
	})
}
func (c *inventoryCatalog) visit(ctx context.Context, lo, hi string, emit func(*cdx.Capture) error) error {
	return c.o.scratch.View(func(tx *badger.Txn) error {
		it := tx.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek([]byte(lo)); it.Valid(); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if string(it.Item().Key()) >= hi {
				break
			}
			b, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			e := &entry{}
			err = c.o.get("x/record/"+string(b), e)
			if err != nil {
				return err
			}
			if err = emit(e.Capture); err != nil {
				return err
			}
		}
		return nil
	})
}
func (c *inventoryCatalog) CapturesByID(ctx context.Context, id string, emit func(*cdx.Capture) error) error {
	return c.visit(ctx, "x/lookup-id/"+hashString(id)+"/", "x/lookup-id/"+hashString(id)+"0", emit)
}
func (c *inventoryCatalog) Scan(ctx context.Context, lo, hi string, emit func(*cdx.Capture) error) error {
	if hi == "" {
		hi = "\xff"
	}
	return c.visit(ctx, "x/lookup-url/"+lo, "x/lookup-url/"+hi, emit)
}
func (o *RedupeOptions) store(key string) *badger.DB {
	if strings.HasPrefix(key, "x/") {
		return o.scratch
	}
	return o.state
}

// The JSON envelope is planning metadata; the capture itself uses the shared
// protobuf codec. No second catalogue contains a duplicate capture value.
type storedEntry struct {
	Capture  []byte `json:"capture"`
	Key      string `json:"key"`
	Metadata *entry `json:"metadata"`
}

func encodeEntry(e *entry) ([]byte, error) {
	b, err := storagecodec.Encode(e.Capture)
	if err != nil {
		return nil, err
	}
	metadata := *e
	metadata.Capture = nil
	return json.Marshal(storedEntry{b, e.Capture.GetSsu(), &metadata})
}
func decodeEntry(b []byte, e *entry) error {
	var stored storedEntry
	if err := json.Unmarshal(b, &stored); err != nil {
		return err
	}
	if stored.Metadata == nil {
		return fmt.Errorf("invalid inventory entry")
	}
	*e = *stored.Metadata
	var err error
	e.Capture, err = storagecodec.Decode(stored.Capture, stored.Key)
	return err
}
