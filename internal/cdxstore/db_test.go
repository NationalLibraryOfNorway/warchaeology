// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/api"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRejectPreviousCanonicalizationProfile(t *testing.T) {
	ctx := t.Context()
	opts := Options{Format: "badger", Dir: t.TempDir()}
	k, err := openKV(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	const previous = "warchaeology-captures-v3;codec-1;whatwg-url-surt-v0.9.1"
	if err := k.Put(ctx, "!format", []byte(previous)); err != nil {
		_ = k.Close()
		t.Fatal(err)
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	for _, readOnly := range []bool{false, true} {
		opts.ReadOnly = readOnly
		db, err := Open(ctx, opts)
		if db != nil {
			_ = db.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "incompatible index database") {
			t.Fatalf("read-only=%v: expected profile rejection, got %v", readOnly, err)
		}
	}
}

// Run the same lifecycle contract against every backend.
func TestBackendContract(t *testing.T) {
	for _, backend := range []string{"badger"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			o := Options{Format: backend, Dir: t.TempDir()}
			db, e := Open(ctx, o)
			if e != nil {
				t.Fatal(e)
			}
			c := func(source, key, id string) *index.Capture {
				return &index.Capture{
					Source:   source,
					Uri:      "https://example.com/",
					Ssu:      key,
					Sts:      timestamppb.New(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
					Rid:      id,
					Ref:      "warcfile:" + source + "#0",
					Filename: "same.warc",
				}
			}
			write := func(source, key, id string, finish bool) {
				t.Helper()
				fileCtx, e := db.BeginFile(ctx, source)
				if e != nil {
					t.Fatal(e)
				}
				if e = db.WriteCapture(fileCtx, c(source, key, id)); e != nil {
					t.Fatal(e)
				}
				if finish {
					if e = db.FinishFile(fileCtx, source, 10, time.Now()); e != nil {
						t.Fatal(e)
					}
				}
			}
			count := func(want int) {
				t.Helper()
				n := 0
				if e := db.Scan(ctx, "", "", func(_ *index.Capture) error { n++; return nil }); e != nil {
					t.Fatal(e)
				}
				if n != want {
					t.Fatalf("got %d captures, want %d", n, want)
				}
			}
			write("/a/same.warc", "com,example)/", "one", true)
			write("/b/same.warc", "com,example)/", "two", true)
			count(2)
			if _, e = db.PathByFilename("same.warc"); e == nil {
				t.Fatal("basename collision accepted")
			}
			write("/a/same.warc", "com,example)/", "one", true)
			count(2) // retry is idempotent
			write("/a/same.warc", "com,example)/changed", "new", false)
			count(2) // no partial publication
			write("/a/same.warc", "com,example)/changed", "new", true)
			count(2)
			if ref, e := db.GetStorageRef(ctx, "one"); e != nil || ref != "" {
				t.Fatalf("stale ID: %q, %v", ref, e)
			}
			q, e := index.ParseQuery(map[string][]string{"url": {"https://example.com/changed"}, "sort": {"closest"}, "closest": {"2020"}})
			if e != nil {
				t.Fatal(e)
			}
			n := 0
			if _, e = q.Run(ctx, db, func(c *index.Capture) error {
				n++
				if c.GetRef() == "" {
					t.Error("sort lost storage reference")
				}
				return nil
			}); e != nil || n != 1 {
				t.Fatalf("closest: count %d, err %v", n, e)
			}
			reportReq, e := api.Parse(map[string][]string{})
			if e != nil {
				t.Fatal(e)
			}
			report, e := db.CreateReport(ctx, reportReq)
			if e != nil {
				t.Fatal(e)
			}
			deadline := time.NewTimer(5 * time.Second)
			ticker := time.NewTicker(time.Millisecond)
			completed := false
			for !completed {
				select {
				case <-deadline.C:
					t.Fatal("report did not finish")
				case <-ticker.C:
					report, e = db.GetReport(ctx, report.GetId())
					if e != nil {
						t.Fatal(e)
					}
					if report.GetStatus() == schema.Report_FAILED {
						t.Fatal(report.GetError())
					}
					completed = report.GetStatus() == schema.Report_COMPLETED
				}
			}
			deadline.Stop()
			ticker.Stop()
			if report.GetData().GetNrOfRecords() != 2 {
				t.Fatalf("report count %d", report.GetData().GetNrOfRecords())
			}
			if e = db.DeleteReport(ctx, report.GetId()); e != nil {
				t.Fatal(e)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if e = db.WriteCapture(canceled, c("x", "x", "x")); !errors.Is(e, context.Canceled) {
				t.Fatalf("cancellation: %v", e)
			}
			if e = db.Close(); e != nil {
				t.Fatal(e)
			}
			o.ReadOnly = true
			db, e = Open(ctx, o)
			if e != nil {
				t.Fatal(e)
			}
			count(2)
			if e = db.WriteCapture(ctx, c("x", "x", "x")); !errors.Is(e, ErrReadOnly) {
				t.Fatalf("read-only: %v", e)
			}
			if e = db.Close(); e != nil {
				t.Fatal(e)
			}
			o.ReadOnly = false
			db, e = Open(ctx, o)
			if e != nil {
				t.Fatal(e)
			}
			defer testcheck.Close(t, db)
			if e = db.Delete(ctx); e != nil {
				t.Fatal(e)
			}
			count(0)
		})
	}
}

func TestSameFileIDOccurrences(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, Options{Format: "badger", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	fileCtx, err := db.BeginFile(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int64{0, 100} {
		c := &index.Capture{
			Source: "source",
			Ssu:    "example)/",
			Sts:    timestamppb.New(time.Now()),
			Rid:    "duplicate",
			Offset: offset,
			Ref:    fmt.Sprintf("warcfile:source#%d", offset),
		}
		if err := db.WriteCapture(fileCtx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.FinishFile(fileCtx, "source", 200, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.StorageRefById(ctx, "duplicate"); err == nil {
		t.Fatal("same-file ID collision was overwritten")
	}
	var n int
	if err := db.CapturesByID(ctx, "duplicate", func(_ *index.Capture) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("got %d occurrences", n)
	}
}

func TestLegacyCatalogueRevisitLookup(t *testing.T) {
	ctx := t.Context()
	opts := Options{Format: "badger", Dir: t.TempDir()}
	db, err := Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	c := &index.Capture{Uri: "https://example.org/", Ssu: "stored-post-key", Sts: timestamppb.Now(), Source: "donor.warc", Ref: "warcfile:donor.warc#0"}
	if err := db.WriteCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	// The previous catalogue format had only the CDX and record-ID indexes.
	if err := db.kv.batch(ctx, []mutation{{key: "!target-index", remove: true}, {key: targetKey(c, captureKey(c)), remove: true}}); err != nil {
		t.Fatal(err)
	}
	testcheck.Close(t, db)
	for _, readOnly := range []bool{true, false, true} {
		opts.ReadOnly = readOnly
		db, err = Open(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		targets, err := db.CapturesByTarget(ctx, c.Uri, c.Sts.AsTime())
		testcheck.Close(t, db)
		if err != nil || len(targets) != 1 || targets[0].GetRef() != c.GetRef() {
			t.Fatalf("readOnly=%v: %v, %v", readOnly, targets, err)
		}
	}
}

func TestURIRecordIDIsExact(t *testing.T) {
	ctx := t.Context()
	d, e := Open(ctx, Options{Format: "badger", Dir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	defer testcheck.Close(t, d)
	for i, id := range []string{"https://records.example/id", "https://records.example/id/nested"} {
		c := &index.Capture{Ssu: "com,example)/", Rid: id, Offset: int64(i), Sts: timestamppb.New(time.Now())}
		if e = d.WriteCapture(ctx, c); e != nil {
			t.Fatal(e)
		}
	}
	n := 0
	if e = d.CapturesByID(ctx, "https://records.example/id", func(c *index.Capture) error {
		n++
		if c.GetRid() != "https://records.example/id" {
			t.Fatal(c.GetRid())
		}
		return nil
	}); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}

func TestRejectPreviousBackup(t *testing.T) {
	db := openTestDB(t)
	if err := db.kv.Put(t.Context(), "!format", []byte("warchaeology-captures-v3;codec-1;whatwg-url-surt-v0.9.1")); err != nil {
		t.Fatal(err)
	}
	var backup bytes.Buffer
	if err := db.Backup(t.Context(), &backup); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if err := Restore(t.Context(), dst, &backup); err == nil || !strings.Contains(err.Error(), rebuildInstructions) {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial restore: %v", err)
	}
}

func TestReindexSameOffsetPreservesCaptureCount(t *testing.T) {
	db, err := Open(t.Context(), Options{Format: "badger", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	const source = "source.warc"
	for range 2 {
		ctx, err := db.BeginFile(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"source.warc", "archive/source.warc"} {
			capture := &index.Capture{
				Uri: "https://example.org/", Ssu: "org,example)/",
				Filename: name, Source: source, Sts: timestamppb.New(time.Unix(100, 0)),
			}
			if err := db.WriteCapture(ctx, capture); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.FinishFile(ctx, source, 100, time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := db.Stats(t.Context())
	if err != nil || stats["captures"] != int64(2) {
		t.Fatalf("stats = %v, error = %v", stats, err)
	}
	rows := 0
	if err := db.kv.Scan(t.Context(), "c/", "c0", func(_ string, _ []byte) error {
		rows++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("found %d stored rows, want 2", rows)
	}
}

type cancelResetBatch struct {
	kv
	cancel context.CancelFunc
}

func (k cancelResetBatch) batch(ctx context.Context, ops []mutation) error {
	err := k.kv.batch(ctx, ops)
	k.cancel()
	return err
}

func TestInterruptedResetRecovery(t *testing.T) {
	ctx := t.Context()
	opts := Options{Format: "badger", Dir: t.TempDir()}
	db, err := Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	write := func() {
		t.Helper()
		const source = "capture.warc"
		fileCtx, err := db.BeginFile(ctx, source)
		if err != nil {
			t.Fatal(err)
		}
		c := &index.Capture{Uri: "https://example.org/", Ssu: "org,example)/", Filename: source, Source: source, Sts: timestamppb.Now()}
		if err = db.WriteCapture(fileCtx, c); err != nil {
			t.Fatal(err)
		}
		if err = db.FinishFile(fileCtx, source, 100, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	write()
	var ops []mutation
	for i := range 300 {
		ops = append(ops, mutation{key: fmt.Sprintf("report/%03d", i), value: []byte("report")})
	}
	if err = db.kv.batch(ctx, ops); err != nil {
		t.Fatal(err)
	}
	reset, cancel := context.WithCancel(ctx)
	defer cancel()
	db.kv = cancelResetBatch{db.kv, cancel}
	if err = db.Delete(reset); !errors.Is(err, context.Canceled) {
		t.Fatalf("reset: %v", err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	opts.ReadOnly = true
	if reader, err := Open(ctx, opts); err == nil {
		testcheck.Close(t, reader)
		t.Fatal("read-only open accepted incomplete reset")
	}
	opts.ReadOnly = false
	db, err = Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	if err = db.kv.Scan(ctx, "report/", "report0", func(string, []byte) error { return errors.New("report survived reset") }); err != nil {
		t.Fatal(err)
	}
	if n, err := db.count(ctx); err != nil || n != 0 {
		t.Fatalf("count=%d error=%v", n, err)
	}
	if rev, err := db.TextRevision(ctx); err != nil || rev == "" {
		t.Fatalf("revision=%q error=%v", rev, err)
	}
	if err = db.Scan(ctx, "", "", func(*index.Capture) error { return errors.New("capture survived reset") }); err != nil {
		t.Fatal(err)
	}
	write()
	if n, err := db.count(ctx); err != nil || n != 1 {
		t.Fatalf("count after reindex=%d error=%v", n, err)
	}

}

func TestReadOnlyLock(t *testing.T) {
	ctx := t.Context()
	opts := Options{Format: "badger", Dir: t.TempDir()}
	open := func(readOnly bool) *DB {
		t.Helper()
		opts.ReadOnly = readOnly
		db, err := Open(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	blocked := func(readOnly bool) {
		t.Helper()
		opts.ReadOnly = readOnly
		if db, err := Open(ctx, opts); err == nil {
			testcheck.Close(t, db)
			t.Fatalf("unexpected concurrent open with readOnly=%t", readOnly)
		}
	}
	writer := open(false)
	blocked(true)
	testcheck.Close(t, writer)
	first := open(true)
	second := open(true)
	testcheck.Close(t, first)
	blocked(false)
	testcheck.Close(t, second)
	testcheck.Close(t, open(false))
}
