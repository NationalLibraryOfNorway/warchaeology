// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package fulltext

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func publishTextCapture(t *testing.T, db *cdxstore.DB, path, key, uri, text string) {
	t.Helper()
	raw := fmt.Sprintf("WARC/1.1\r\nWARC-Type: resource\r\nWARC-Record-ID: <urn:uuid:1789da6c-6444-4f9a-b5ca-cb591e39713a>\r\nWARC-Date: 2026-01-01T20:15:59Z\r\nWARC-Target-URI: %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s\r\n\r\n", uri, len(text), text)
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	generation, err := db.BeginFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		err = db.WriteCapture(generation, &cdx.Capture{
			Uri:    uri,
			Ssu:    key,
			Mct:    "Text/Plain; charset=UTF-8",
			Source: path,
			Ref:    "warcfile:" + path + "#0",
			Sts:    timestamppb.New(time.Date(2026, 1, 1, 20, 15, 59, 0, time.UTC)),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.FinishFile(generation, path, info.Size(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestSyncVisibilityReplacementDeletionAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := cdxstore.Open(ctx, cdxstore.Options{Format: "badger", Dir: filepath.Join(dir, "captures")})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	path := filepath.Join(dir, "text.warc")
	uri := "https://example.org/"
	publish := func(text string) {
		publishTextCapture(t, db, path, "stored-text-key", uri, text)
	}
	publish("northern lights")
	upload := `unused 20260101201559 {"url":"urn:text:https://example.org/remote","filename":"remote.warc","offset":"0","length":"10","mime":"text/plain"}`
	if _, err := db.Upload(ctx, strings.NewReader(upload), cdxstore.UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dir, "text")
	idx, err := Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an existing index that skipped this capture under the old rules.
	revision, err := db.TextRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.index.SetInternal([]byte("revision"), []byte("text-v4:"+revision)); err != nil {
		t.Fatal(err)
	}
	syncer := &Synchronizer{Index: idx, DB: db}
	if err = syncer.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	result, err := idx.Search(ctx, Request{Query: "northern"})
	if err != nil || len(result.Hits) != 1 {
		t.Fatalf("search=%+v error=%v", result, err)
	}
	old := result.Hits[0].Fields
	if !syncer.Visible(ctx, old) {
		t.Fatal("published capture invisible")
	}
	publish("southern lights")
	if syncer.Visible(ctx, old) {
		t.Fatal("obsolete generation still visible")
	}
	if err = syncer.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	result, err = idx.Search(ctx, Request{Query: "northern"})
	if err != nil || result.Total != 0 {
		t.Fatalf("old text retained: %+v %v", result, err)
	}
	if err = idx.Close(); err != nil {
		t.Fatal(err)
	}
	idx, err = Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)
	syncer = &Synchronizer{Index: idx, DB: db}
	if err = syncer.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if syncer.Status().Indexed != 0 {
		t.Fatal("unchanged revision was reindexed")
	}
	publish("")
	if err = syncer.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	result, err = idx.Search(ctx, Request{Query: "lights"})
	if err != nil || result.Total != 0 {
		t.Fatalf("deleted text retained: %+v %v", result, err)
	}
}

func TestSyncIndexesAvailableArchivesWhenOneIsMissing(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	db, err := cdxstore.Open(ctx, cdxstore.Options{Format: "badger", Dir: filepath.Join(dir, "captures")})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	idx, err := Open(filepath.Join(dir, "text"))
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)
	syncer := &Synchronizer{Index: idx, DB: db}
	first := filepath.Join(dir, "first.warc")
	publishTextCapture(t, db, first, "a", "https://example.org/a", "northern lights")
	if err := syncer.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	moved := first + ".offline"
	if err := os.Rename(first, moved); err != nil {
		t.Fatal(err)
	}
	publishTextCapture(t, db, filepath.Join(dir, "second.warc"), "b", "https://example.org/b", "southern lights")
	if err := syncer.Sync(ctx, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sync with missing archive: %v", err)
	}
	if status := syncer.Status(); status.Indexed != 1 || !strings.Contains(status.Error, first) {
		t.Fatalf("partial sync status: %+v", status)
	}
	for _, term := range []string{"northern", "southern"} {
		result, err := idx.Search(ctx, Request{Query: term})
		if err != nil || len(result.Hits) != 1 || !syncer.Visible(ctx, result.Hits[0].Fields) {
			t.Fatalf("search for %q after partial sync: %+v, %v", term, result, err)
		}
	}
	if err := syncer.Sync(ctx, false); !errors.Is(err, os.ErrNotExist) || syncer.Status().Indexed != 0 {
		t.Fatalf("retry of missing archive: status=%+v err=%v", syncer.Status(), err)
	}
	if err := os.Rename(moved, first); err != nil {
		t.Fatal(err)
	}
	if err := syncer.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if status := syncer.Status(); status.Indexed != 1 || status.Error != "" {
		t.Fatalf("retry status: %+v", status)
	}
	result, err := idx.Search(ctx, Request{Query: "southern"})
	if err != nil || result.Total != 1 {
		t.Fatalf("completed source lost after recovery: %+v %v", result, err)
	}
}

func TestPhraseAndTimelineCoverAllMatches(t *testing.T) {
	idx, err := Open(filepath.Join(t.TempDir(), "text"))
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var docs []Document
	for n := range 60 {
		docs = append(docs, Document{
			ID:        fmt.Sprint(n),
			URL:       "https://example.org/",
			Text:      "northern lights",
			Timestamp: start.Add(time.Duration(n) * time.Hour),
		})
	}
	docs = append(docs, Document{ID: "different", URL: "https://example.org/", Text: "lights in the northern sky", Timestamp: start})
	if err = idx.Publish(t.Context(), docs, nil); err != nil {
		t.Fatal(err)
	}
	result, err := idx.Search(t.Context(), Request{Query: `"northern lights"`, Limit: 5})
	if err != nil || result.Total != 60 || len(result.Hits) != 5 {
		t.Fatalf("phrase search %+v %v", result, err)
	}
	timeline, err := idx.Timeline(t.Context(), Request{Query: `"northern lights"`})
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, count := range timeline.Counts {
		total += count
	}
	if total != 60 {
		t.Fatalf("histogram has %d matches", total)
	}
}

func TestTimelineFiltersMatchBucketCounts(t *testing.T) {
	idx, err := Open(filepath.Join(t.TempDir(), "index"))
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var docs []Document
	for _, sec := range []int{0, 1, 48} {
		docs = append(docs, Document{ID: fmt.Sprint(sec), URL: "https://example.org/", Text: "test", Timestamp: start.Add(time.Duration(sec) * time.Second)})
	}
	if err := idx.Publish(t.Context(), docs, nil); err != nil {
		t.Fatal(err)
	}
	tl, err := idx.Timeline(t.Context(), Request{Query: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for i, count := range tl.Counts {
		if count == 0 {
			continue
		}
		result, err := idx.Search(t.Context(), Request{Query: "test", From: tl.Bounds[i], To: tl.Bounds[i+1]})
		if err != nil {
			t.Fatal(err)
		}
		if result.Total != count {
			t.Errorf("bucket %d shows %d captures, filter found %d", i, count, result.Total)
		}
	}
}
