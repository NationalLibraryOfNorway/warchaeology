package cdxstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/api"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"google.golang.org/protobuf/types/known/timestamppb"
)

//nolint:gocyclo // Run the same lifecycle contract against every backend.
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
				return &index.Capture{Source: source, Uri: "https://example.com/", Ssu: key, Sts: timestamppb.New(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)), Rid: id, Ref: "warcfile:" + source + "#0", Filename: "same.warc"}
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
			defer db.Close()
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
	defer db.Close()
	fileCtx, err := db.BeginFile(ctx, "source")
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int64{0, 100} {
		c := &index.Capture{Source: "source", Ssu: "example)/", Sts: timestamppb.New(time.Now()), Rid: "duplicate", Offset: offset, Ref: fmt.Sprintf("warcfile:source#%d", offset)}
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
