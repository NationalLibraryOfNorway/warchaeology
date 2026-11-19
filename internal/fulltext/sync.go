// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package fulltext

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/blevesearch/bleve/v2"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
)

type Status struct {
	Running bool      `json:"running"`
	Indexed int       `json:"indexed"`
	Error   string    `json:"error,omitempty"`
	Updated time.Time `json:"updated,omitempty"`
}

// Synchronizer snapshots only candidate metadata to disk, releases the capture
// publication lock, then reads archive payloads. The saved revision advances
// only after successful indexing and removal of obsolete documents.
type Synchronizer struct {
	Index  *Index
	DB     *cdxstore.DB
	mu     sync.Mutex
	status Status
	runMu  sync.Mutex
}

func (s *Synchronizer) Status() Status { s.mu.Lock(); defer s.mu.Unlock(); return s.status }
func (s *Synchronizer) Sync(ctx context.Context, force bool) (err error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	revision, err := s.DB.TextRevision(ctx)
	if err != nil {
		return err
	}
	// A version prefix also backfills databases created before revision tracking.
	revision = "text-v5:" + revision
	previous, err := s.Index.index.GetInternal([]byte("revision"))
	if err != nil {
		return err
	}
	if !force && string(previous) == revision {
		return nil
	}
	s.mu.Lock()
	s.status = Status{Running: true}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.status.Running = false
		s.status.Updated = time.Now()
		if err != nil {
			s.status.Error = err.Error()
		}
	}()
	spool, err := os.CreateTemp("", "warc-text-*.jsonl")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.Remove(spool.Name())) }()
	defer func() { err = errors.Join(err, spool.Close()) }()
	var packages []cdxstore.PackageInfo
	members := map[string]bool{}
	if err = s.DB.ListPackages(ctx, func(p cdxstore.PackageInfo) error {
		if p.Fingerprint != "" {
			p.Manifest = nil
			p.Children = nil
			packages = append(packages, p)
			for _, source := range p.Sources {
				members[source] = true
			}
		}
		return nil
	}); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "warc-text-stage-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	stage, err := cdxstore.OpenCaptureStage(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, stage.Close()) }()
	enc := json.NewEncoder(spool)
	err = s.DB.Scan(ctx, "", "", func(c *cdx.Capture) error {
		if plainText(c.GetMct()) && c.GetRef() != "" {
			if members[c.GetSource()] {
				return stage.WriteCapture(ctx, c)
			}
			return enc.Encode(c)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err = spool.Seek(0, 0); err != nil {
		return err
	}
	if err = stage.Flush(ctx); err != nil {
		return err
	}
	run := uuid.NewV7().String()
	dec := json.NewDecoder(spool)
	batch := make([]Document, 0, 64)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if e := s.Index.Publish(ctx, batch, nil); e != nil {
			return e
		}
		s.mu.Lock()
		s.status.Indexed += len(batch)
		s.mu.Unlock()
		clear(batch)
		batch = batch[:0]
		return nil
	}
	var bytes int
	emit := func(doc Document) error {
		if len(batch) > 0 && bytes+len(doc.Text) > 16<<20 {
			if err := flush(); err != nil {
				return err
			}
			bytes = 0
		}
		batch = append(batch, doc)
		bytes += len(doc.Text)
		if len(batch) == 64 {
			bytes = 0
			return flush()
		}
		return nil
	}
	records := loader.FileStorageLoader{}
	extract := func(c *cdx.Capture, meta *packagePages) error {
		doc, e := Extract(ctx, records, c, 16<<20)
		if e == nil && doc == nil {
			return nil
		}
		if e != nil {
			return e
		}
		associate(doc, meta)
		doc.SyncRun = run
		return emit(*doc)
	}
	for _, p := range packages {
		meta := &packagePages{id: idFor(p.Locator), fingerprint: p.Fingerprint, pages: map[string][]*page{}}
		if e := readPackagePages(ctx, p, meta); e != nil {
			return e
		}
		for _, source := range p.Sources {
			if e := stage.ScanSource(ctx, source, func(c *cdx.Capture) error { return extract(c, meta) }); e != nil {
				return e
			}
		}
		if err = inlineDocuments(meta, run, emit); err != nil {
			return err
		}
	}
	for {
		var c cdx.Capture
		err = dec.Decode(&c)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err = extract(&c, nil); err != nil {
			return err
		}
	}

	if err = flush(); err != nil {
		return err
	}
	// Use repeated first pages: deleting a page shifts later matches forward.
	old := bleve.NewBooleanQuery()
	all := bleve.NewMatchAllQuery()
	current := bleve.NewTermQuery(run)
	current.SetField("syncRun")
	old.AddMust(all)
	old.AddMustNot(current)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		result, e := s.Index.index.SearchInContext(ctx, bleve.NewSearchRequestOptions(old, 128, 0, false))
		if e != nil {
			return e
		}
		if len(result.Hits) == 0 {
			break
		}
		ids := make([]string, 0, len(result.Hits))
		for _, h := range result.Hits {
			ids = append(ids, h.ID)
		}
		if err = s.Index.Publish(ctx, nil, ids); err != nil {
			return err
		}
	}
	return s.Index.index.SetInternal([]byte("revision"), []byte(revision))
}

// Visible suppresses obsolete hits while a replacement or deletion is pending.
func (s *Synchronizer) Visible(ctx context.Context, fields map[string]interface{}) bool {
	uri, _ := fields["captureURI"].(string)
	ref, _ := fields["ref"].(string)
	generation, _ := fields["generation"].(string)
	if uri == "" && strings.HasPrefix(ref, "page:") {
		id, _ := fields["package"].(string)
		found := false
		err := s.DB.ListPackages(ctx, func(p cdxstore.PackageInfo) error {
			if p.SourceFingerprint != "" && idFor(p.Locator) == id && p.Fingerprint == generation {
				found = true
			}
			return nil
		})
		return err == nil && found
	}
	if uri == "" || ref == "" {
		return false
	}
	key, _ := fields["captureKey"].(string)
	if key == "" {
		key = cdx.CanonicalKey(uri)
	}
	key += " "
	found := false
	err := s.DB.Scan(ctx, key, key+"\xff", func(c *cdx.Capture) error {
		if c.GetRef() == ref && c.GetGeneration() == generation {
			found = true
		}
		return nil
	})
	return err == nil && found
}
