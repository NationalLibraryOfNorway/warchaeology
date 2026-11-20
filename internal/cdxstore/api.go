// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"uuid"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/keyvalue"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"google.golang.org/protobuf/proto"
)

func send[T any](ctx context.Context, ch chan<- T, v T) error {
	select {
	case ch <- v:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *DB) Search(ctx context.Context, req index.Request, out chan<- index.CdxResponse) error {
	return d.runAsync(ctx, func(ctx context.Context) {
		defer close(out)
		q := queryFromRequest(req)
		var uri string
		if req.MatchType() == index.MatchTypeVerbatim {
			uri = req.OriginalURI()
		}
		q.Predicate = func(c *index.Capture) bool {
			if uri != "" && c.GetUri() != uri {
				return false
			}
			if dr := req.DateRange(); dr != nil && !dr.Contains(c.GetSts().AsTime().Unix()) {
				return false
			}
			if f := req.Filter(); f != nil && !f.Eval(c) {
				return false
			}
			return true
		}
		_, e := q.Run(ctx, d, func(c *index.Capture) error {
			return send[index.CdxResponse](ctx, out, keyvalue.CdxResponse{Key: keyvalue.CdxKey(captureKey(c)[2:]), Value: c})
		})
		if e != nil {
			_ = send[index.CdxResponse](ctx, out, keyvalue.CdxResponse{Error: e})
		}
	})
}

func (d *DB) ListFileInfo(ctx context.Context, req index.Request, out chan<- index.FileInfoResponse) error {
	return d.runAsync(ctx, func(ctx context.Context) {
		defer close(out)
		count := 0
		e := d.kv.Scan(ctx, "s/", "s0", func(_ string, v []byte) error {
			if req.Limit() > 0 && count >= req.Limit() {
				return errStop
			}
			var cp checkpoint
			if e := json.Unmarshal(v, &cp); e != nil {
				return e
			}
			count++
			return send[index.FileInfoResponse](ctx, out, keyvalue.FileInfoResponse{FileInfo: cp.File})
		})
		if e != nil && !errors.Is(e, errStop) {
			_ = send[index.FileInfoResponse](ctx, out, keyvalue.FileInfoResponse{Error: e})
		}
	})
}

func (d *DB) ListStorageRef(ctx context.Context, req index.Request, out chan<- index.IdResponse) error {
	return d.runAsync(ctx, func(ctx context.Context) {
		defer close(out)
		n := 0
		d.visibility.RLock()
		defer d.visibility.RUnlock()
		e := d.kv.Scan(ctx, "i/", "i0", func(_ string, v []byte) error {
			if req.Limit() > 0 && n >= req.Limit() {
				return errStop
			}
			raw, e := d.kv.Get(ctx, string(v))
			if e != nil {
				return e
			}
			if len(raw) == 0 {
				return nil
			}
			c, e := decodeCapture(string(v), raw)
			if e != nil {
				return e
			}
			ok, e := d.currentIDCapture(ctx, c)
			if e != nil {
				return e
			}
			if !ok {
				return nil
			}
			n++
			return send[index.IdResponse](ctx, out, keyvalue.IdResponse{Key: c.GetRid(), Value: c.GetRef()})
		})
		if e != nil && !errors.Is(e, errStop) {
			_ = send[index.IdResponse](ctx, out, keyvalue.IdResponse{Error: e})
		}
	})
}

func (d *DB) Debug(ctx context.Context, req keyvalue.DebugRequest, out chan<- keyvalue.CdxResponse) error {
	return d.runAsync(ctx, func(ctx context.Context) {
		defer close(out)
		e := d.Scan(ctx, req.Key, "", func(c *index.Capture) error {
			return send(ctx, out, keyvalue.CdxResponse{Key: keyvalue.CdxKey(captureKey(c)[2:]), Value: c})
		})
		if e != nil {
			_ = send(ctx, out, keyvalue.CdxResponse{Error: e})
		}
	})
}

func (d *DB) CreateReport(ctx context.Context, req index.Request) (*schema.Report, error) {
	if d.readOnly || d.indexOnly {
		return nil, ErrReadOnly
	}
	g, e := keyvalue.NewReportGenerator(d)
	if e != nil {
		return nil, e
	}
	return g.Generate(ctx, req)
}

func (d *DB) SaveReport(ctx context.Context, r *schema.Report) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deleted[r.GetId()] {
		return context.Canceled
	}
	if d.readOnly {
		return ErrReadOnly
	}
	b, e := proto.Marshal(r)
	if e != nil {
		return e
	}
	return d.kv.Put(ctx, "r/"+r.GetId(), b)
}

func (d *DB) GetReport(ctx context.Context, id string) (*schema.Report, error) {
	b, e := d.kv.Get(ctx, "r/"+id)
	if e != nil {
		return nil, e
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("report not found")
	}
	r := &schema.Report{}
	return r, proto.Unmarshal(b, r)
}

func (d *DB) ListReports(ctx context.Context, req index.Request, out chan<- index.ReportResponse) error {
	return d.runAsync(ctx, func(ctx context.Context) {
		defer close(out)
		e := d.kv.Scan(ctx, "r/", "r0", func(_ string, b []byte) error {
			r := &schema.Report{}
			if e := proto.Unmarshal(b, r); e != nil {
				return e
			}
			return send[index.ReportResponse](ctx, out, keyvalue.ReportResponse{Value: r})
		})
		if e != nil {
			_ = send[index.ReportResponse](ctx, out, keyvalue.ReportResponse{Error: e})
		}
	})
}

func (d *DB) CancelReport(_ context.Context, id string) error {
	if d.readOnly {
		return ErrReadOnly
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.tasks[id]; c != nil {
		c()
	}
	return nil
}

func (d *DB) DeleteReport(ctx context.Context, id string) error {
	if d.readOnly {
		return ErrReadOnly
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleted[id] = true
	if c := d.tasks[id]; c != nil {
		c()
	}
	return d.kv.Delete(ctx, "r/"+id)
}

func (d *DB) AddTask(id string, cancel context.CancelFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.wg.Add(1)
	d.tasks[id] = cancel
}

func (d *DB) DeleteTask(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.tasks, id)
	d.wg.Done()
}

var _ index.API = (*DB)(nil)

func (d *DB) runAsync(ctx context.Context, fn func(context.Context)) error {
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return fmt.Errorf("database closed")
	}
	ctx, cancel := context.WithCancel(ctx)
	id := "scan-" + uuid.NewV7().String()
	d.tasks[id] = cancel
	d.wg.Add(1)
	d.mu.Unlock()
	go func() { defer d.DeleteTask(id); defer cancel(); fn(ctx) }()
	return nil
}
