// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/storagecodec"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
)

const uploadLine = `ignored 20200102030405 {"url":"https://example.com/","filename":"a.warc.gz","offset":"0","length":"100","custom":9007199254740993}`

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, e := Open(t.Context(), Options{Format: "badger", Dir: t.TempDir(), IndexOnly: true})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}
func exportTest(t *testing.T, d *DB) string {
	t.Helper()
	var b bytes.Buffer
	if e := d.Export(t.Context(), &b); e != nil {
		t.Fatal(e)
	}
	return b.String()
}
func TestUploadLifecycle(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	upload := func(s string, o UploadOptions) error { _, e := d.Upload(ctx, strings.NewReader(s), o); return e }
	if e := upload(uploadLine+"\ninvalid", UploadOptions{}); e == nil {
		t.Fatal("invalid strict upload accepted")
	}
	if got := exportTest(t, d); got != "" {
		t.Fatal(got)
	}
	r, e := d.Upload(ctx, strings.NewReader(uploadLine+"\ninvalid"), UploadOptions{SkipBad: true})
	if e != nil || r.Accepted != 1 || r.Rejected != 1 || r.Diagnostics[0].Line != 2 {
		t.Fatalf("%+v %v", r, e)
	}
	before := exportTest(t, d)
	if !strings.Contains(before, "9007199254740993") || !strings.HasPrefix(before, "com,example)/") {
		t.Fatal(before)
	}
	if e = upload(uploadLine, UploadOptions{}); e != nil {
		t.Fatal(e)
	}
	if exportTest(t, d) != before {
		t.Fatal("retry changed collection")
	}
	different := strings.Replace(uploadLine, `"offset":"0"`, `"offset":"1"`, 1)
	if e = upload(different, UploadOptions{}); e != nil {
		t.Fatal(e)
	}
	if strings.Count(exportTest(t, d), "\n") != 2 {
		t.Fatal("distinct location lost")
	}
	conflict := strings.Replace(uploadLine, `"length":"100"`, `"length":"101"`, 1)
	if e = upload(uploadLine+"\n"+conflict, UploadOptions{}); e == nil {
		t.Fatal("conflict accepted")
	}
	if e = upload(uploadLine, UploadOptions{Delete: true}); e != nil {
		t.Fatal(e)
	}
	if e = upload(uploadLine, UploadOptions{Delete: true}); e != nil {
		t.Fatal(e)
	}
	if strings.Count(exportTest(t, d), "\n") != 1 {
		t.Fatal("delete removed another location")
	}
	stats, e := d.Stats(ctx)
	if e != nil || stats["captures"] != int64(1) {
		t.Fatalf("%v %v", stats, e)
	}
	var backup bytes.Buffer
	if e = d.Backup(ctx, &backup); e != nil {
		t.Fatal(e)
	}
	path := t.TempDir() + "/restored"
	if e = Restore(ctx, path, &backup); e != nil {
		t.Fatal(e)
	}
	restored, e := Open(ctx, Options{Format: "badger", Dir: path})
	if e != nil {
		t.Fatal(e)
	}
	defer testcheck.Close(t, restored)
	if exportTest(t, restored) != exportTest(t, d) {
		t.Fatal("backup changed captures")
	}
	if !restored.IndexOnly() {
		t.Fatal("backup lost capabilities")
	}
}

type brokenKV struct {
	kv

	remaining int
}

func (b *brokenKV) batch(ctx context.Context, ops []mutation) error {
	b.remaining--
	if b.remaining < 0 {
		return syscall.ENOSPC
	}
	return b.kv.batch(ctx, ops)
}
func TestUploadDiskFailureAndCancellation(t *testing.T) {
	d := openTestDB(t)
	original := d.kv
	d.kv = &brokenKV{original, 1}
	_, e := d.Upload(t.Context(), strings.NewReader(uploadLine), UploadOptions{})
	if !errors.Is(e, syscall.ENOSPC) {
		t.Fatalf("%v", e)
	}
	if exportTest(t, d) != "" {
		t.Fatal("unpublished upload visible")
	}
	d.kv = original
	if e = d.recoverUploads(t.Context()); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e = d.Upload(ctx, strings.NewReader(uploadLine), UploadOptions{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestInterruptedPublicationRecovery(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprint(published), func(t *testing.T) {
			d := openTestDB(t)
			c, e := cdx.ParseIngestLine([]byte(uploadLine))
			if e != nil {
				t.Fatal(e)
			}
			c.Generation = "upload-test"
			base := captureBase(c)
			raw, e := storagecodec.Encode(c)
			if e != nil {
				t.Fatal(e)
			}
			ops := []mutation{{key: base + c.GetGeneration(), value: raw}, {key: "j/" + c.GetGeneration() + "/" + base, value: []byte(base)}}
			if published {
				ops = append(ops, mutation{key: "u/" + c.GetGeneration(), value: []byte{1}}, mutation{key: "!count", value: []byte("1")})
			}
			if e = d.batch(t.Context(), ops); e != nil {
				t.Fatal(e)
			}
			if (exportTest(t, d) != "") != published {
				t.Fatal("wrong visibility")
			}
			dir := d.dir
			if e = d.Close(); e != nil {
				t.Fatal(e)
			}
			d, e = Open(t.Context(), Options{Format: "badger", Dir: dir})
			if e != nil {
				t.Fatal(e)
			}
			defer testcheck.Close(t, d)
			if (exportTest(t, d) != "") != published {
				t.Fatal("wrong recovered visibility")
			}
			n := 0
			_ = d.kv.Scan(t.Context(), "j/", "j0", func(string, []byte) error { n++; return nil })
			if n != 0 {
				t.Fatal("journal not reclaimed")
			}
		})
	}
}
func TestOrderedSeeksAndCaps(t *testing.T) {
	d := openTestDB(t)
	for _, year := range []string{"2019", "2020", "2021"} {
		line := strings.Replace(uploadLine, "20200102", year+"0102", 1)
		if _, e := d.Upload(t.Context(), strings.NewReader(line), UploadOptions{}); e != nil {
			t.Fatal(e)
		}
	}
	for _, order := range []string{"closest", "reverse"} {
		q, e := cdx.ParseQuery(map[string][]string{"url": {"https://example.com/"}, "sort": {order}, "closest": {"2020"}})
		if e != nil {
			t.Fatal(e)
		}
		q.Limit = 1
		q.State = &cdx.QueryState{}
		var got string
		_, e = q.Run(t.Context(), d, func(c *cdx.Capture) error { got = c.GetSts().AsTime().Format("2006"); return nil })
		want := "2020"
		if order == "reverse" {
			want = "2021"
		}
		if e != nil || got != want || !q.State.Truncated {
			t.Fatalf("%s: %s %+v %v", order, got, q.State, e)
		}
	}
}

func TestNativeDeletionAndReindex(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	c, e := cdx.ParseIngestLine([]byte(uploadLine))
	if e != nil {
		t.Fatal(e)
	}
	c.Source = "native-source"
	c.Rid = "record"
	c.Ref = "warcfile:native-source#0"
	write := func() {
		t.Helper()
		g, e := d.BeginFile(ctx, c.GetSource())
		if e != nil {
			t.Fatal(e)
		}
		if e = d.WriteCapture(g, c); e != nil {
			t.Fatal(e)
		}
		if e = d.FinishFile(g, c.GetSource(), 100, c.GetSts().AsTime()); e != nil {
			t.Fatal(e)
		}
	}
	write()
	if _, e = d.Upload(ctx, strings.NewReader(uploadLine), UploadOptions{Delete: true}); e != nil {
		t.Fatal(e)
	}
	if exportTest(t, d) != "" {
		t.Fatal("native capture was not deleted")
	}
	write()
	stats, e := d.Stats(ctx)
	if e != nil || stats["captures"] != int64(1) {
		t.Fatalf("%v %v", stats, e)
	}
	if _, e = d.Upload(ctx, strings.NewReader(uploadLine), UploadOptions{}); e != nil {
		t.Fatal(e)
	}
	if strings.Count(exportTest(t, d), "\n") != 1 {
		t.Fatal("upsert duplicated native identity")
	}
	write()
	if strings.Count(exportTest(t, d), "\n") != 1 {
		t.Fatal("native reindex duplicated imported identity")
	}
}

func TestBoundedBatchesWithLargeProperties(t *testing.T) {
	d := openTestDB(t)
	var input strings.Builder
	for i := range 24 {
		fmt.Fprintf(&input, "ignored 20200102030405 {\"url\":\"https://example.com/\",\"filename\":\"a.warc.gz\",\"offset\":%d,\"length\":1,\"custom\":%q}\n", i, strings.Repeat("x", 512<<10))
	}
	r, e := d.Upload(t.Context(), strings.NewReader(input.String()), UploadOptions{})
	if e != nil || r.Accepted != 24 {
		t.Fatalf("%+v %v", r, e)
	}
}

func TestAdjacentCollapseAndScanLimit(t *testing.T) {
	d := openTestDB(t)
	for i := range 5 {
		line := strings.Replace(uploadLine, `"offset":"0"`, fmt.Sprintf(`"offset":"%d"`, i), 1)
		if _, e := d.Upload(t.Context(), strings.NewReader(line), UploadOptions{}); e != nil {
			t.Fatal(e)
		}
	}
	q, e := cdx.ParseQuery(map[string][]string{"url": {"https://example.com/"}, "collapse": {"urlkey"}, "collapseTo": {"last"}})
	if e != nil {
		t.Fatal(e)
	}
	n := 0
	_, e = q.Run(t.Context(), d, func(c *cdx.Capture) error {
		n++
		if c.GetOffset() != 4 {
			t.Fatal(c.GetOffset())
		}
		return nil
	})
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
	q.Collapse = ""
	q.ScanLimit = 2
	q.State = &cdx.QueryState{}
	_, e = q.Run(t.Context(), d, func(*cdx.Capture) error { return nil })
	if e != nil || !q.State.Truncated || q.State.Returned != 2 || q.State.Scanned != 2 {
		t.Fatal(q.State, e)
	}
}

type pauseKV struct {
	kv

	ready, release chan struct{}
	once           bool
}

func (p *pauseKV) batch(ctx context.Context, ops []mutation) error {
	e := p.kv.batch(ctx, ops)
	if !p.once {
		p.once = true
		close(p.ready)
		<-p.release
	}
	return e
}
func TestReadersSeeOnlyPublishedUploads(t *testing.T) {
	d := openTestDB(t)
	p := &pauseKV{kv: d.kv, ready: make(chan struct{}), release: make(chan struct{})}
	d.kv = p
	done := make(chan error, 1)
	go func() { _, e := d.Upload(t.Context(), strings.NewReader(uploadLine), UploadOptions{}); done <- e }()
	<-p.ready
	if exportTest(t, d) != "" {
		t.Fatal("partially staged capture visible")
	}
	close(p.release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if exportTest(t, d) == "" {
		t.Fatal("published capture missing")
	}
}

func TestOversizeLineCanBeSkipped(t *testing.T) {
	d := openTestDB(t)
	input := strings.Repeat("x", 9000) + "\n" + uploadLine + "\n"
	r, e := d.Upload(t.Context(), strings.NewReader(input), UploadOptions{SkipBad: true, MaxLine: 512})
	if e != nil || r.Accepted != 1 || r.Rejected != 1 || r.Diagnostics[0].Line != 1 {
		t.Fatalf("%+v %v", r, e)
	}
}

func TestCanceledPublicationWait(t *testing.T) {
	d := openTestDB(t)
	d.visibility.RLock()
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, e := d.Upload(ctx, strings.NewReader(uploadLine), UploadOptions{})
		done <- e
	}()
	<-started
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	d.visibility.RUnlock()
	if exportTest(t, d) != "" {
		t.Fatal("canceled upload published")
	}
}

func TestOverlappingNativePublications(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	source := "a.warc.gz"
	stage := func() context.Context {
		t.Helper()
		g, err := d.BeginFile(ctx, source)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 300 {
			c, err := cdx.ParseIngestLine([]byte(uploadLine))
			if err != nil {
				t.Fatal(err)
			}
			c.Source, c.Offset = source, int64(i)
			c.Sts.Nanos = 123456789
			if err := d.WriteCapture(g, c); err != nil {
				t.Fatal(err)
			}
		}
		return g
	}
	check := func() {
		t.Helper()
		stats, err := d.Stats(ctx)
		if err != nil || stats["captures"] != int64(300) {
			t.Fatalf("stats %v, %v", stats, err)
		}
		if n := strings.Count(exportTest(t, d), "\n"); n != 300 {
			t.Fatalf("exported %d captures", n)
		}
	}
	first, second := stage(), stage()
	if err := d.FinishFile(first, source, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	check()
	// The upload replaces one published capture while the second writer has
	// flushed captures with the very same source and offsets.
	if _, err := d.Upload(ctx, strings.NewReader(uploadLine), UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	check()
	if err := d.FinishFile(second, source, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	check()
	firstLine, _, _ := strings.Cut(exportTest(t, d), "\n")
	if _, err := d.Upload(ctx, strings.NewReader(firstLine), UploadOptions{Delete: true}); err != nil {
		t.Fatal(err)
	}
	var offsets []int64
	if err := d.Scan(ctx, "", "", func(c *cdx.Capture) error {
		if c.GetSts().GetNanos() != 123456789 {
			t.Fatal("lost native timestamp")
		}
		offsets = append(offsets, c.GetOffset())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(offsets) != 299 || offsets[0] != 1 || offsets[298] != 299 {
		t.Fatalf("unexpected offsets after fractional-second delete: %v", offsets)
	}
	stats, err := d.Stats(ctx)
	if err != nil || stats["captures"] != int64(299) {
		t.Fatalf("stats %v, %v", stats, err)
	}
}

func TestRecordIDDuringPendingCleanup(t *testing.T) {
	ctx := t.Context()
	opts := Options{Format: "badger", Dir: t.TempDir()}
	db, err := Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.warc.gz", "unrelated.warc.gz"} {
		c, err := cdx.ParseIngestLine([]byte(strings.Replace(uploadLine, "a.warc.gz", name, 1)))
		if err != nil {
			t.Fatal(err)
		}
		c.Source = name
		c.Rid = "shared"
		c.Ref = "warcfile:" + name + "#0"
		g, err := db.BeginFile(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.WriteCapture(g, c); err != nil {
			t.Fatal(err)
		}
		if err = db.FinishFile(g, name, 100, c.GetSts().AsTime()); err != nil {
			t.Fatal(err)
		}
	}
	original := db.kv
	db.kv = &brokenKV{original, 2}
	_, err = db.Upload(ctx, strings.NewReader(uploadLine), UploadOptions{Delete: true})
	db.kv = original
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		c, err := db.StorageRefById(ctx, "shared")
		if err != nil || c != "warcfile:unrelated.warc.gz#0" {
			t.Fatal(c, err)
		}
	}
	check()
	testcheck.Close(t, db)
	for _, readOnly := range []bool{true, false} {
		opts.ReadOnly = readOnly
		db, err = Open(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		check()
		testcheck.Close(t, db)
	}
}
