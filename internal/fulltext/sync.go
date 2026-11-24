// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package fulltext

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	Updated time.Time `json:"updated"`
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
	// A partial run retries failed WARCs or whole packages (page associations
	// span members). Reuse its run ID so final cleanup retains completed work.
	retryRevision, retryRun string
	pending                 map[string]bool
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
	if force || revision != s.retryRevision {
		s.pending = nil
	}
	if s.pending == nil && !force && string(previous) == revision {
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
	members := map[string]string{}
	if err = s.DB.ListPackages(ctx, func(p cdxstore.PackageInfo) error {
		if p.Fingerprint != "" {
			p.Manifest = nil
			p.Children = nil
			packages = append(packages, p)
			for _, source := range p.Sources {
				members[source] = p.Locator
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
	enc := jsontext.NewEncoder(spool)
	err = s.DB.Scan(ctx, "", "", func(c *cdx.Capture) error {
		if plainText(c.GetMct()) && c.GetRef() != "" {
			source := c.GetSource()
			pkg := members[source]
			if pkg != "" {
				source = pkg
			}
			if s.pending != nil && !s.pending[source] && !s.pending[c.GetRef()] {
				return nil
			}
			if pkg != "" {
				return stage.WriteCapture(ctx, c)
			}
			return json.MarshalEncode(enc, c)
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
	run := s.retryRun
	if s.pending == nil {
		run = uuid.NewV7().String()
	}
	failed := map[string]bool{}
	dec := jsontext.NewDecoder(spool)
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
	var firstArchiveError error
	archiveErrors := 0
	reportArchiveError := func(location string, cause error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if firstArchiveError == nil {
			firstArchiveError = fmt.Errorf("%s: %w", location, cause)
		}
		archiveErrors++
		return nil
	}
	records := loader.FileStorageLoader{ValidateSource: s.DB.ValidateArchiveSource}
	extract := func(c *cdx.Capture, meta *packagePages, source string) error {
		doc, e := Extract(loader.WithCapture(ctx, c), records, c, 16<<20)
		if errors.Is(e, errNoTextResource) {
			return nil
		}
		if e != nil {
			if meta == nil && !errors.Is(e, os.ErrNotExist) {
				failed[c.GetRef()] = true
			} else {
				failed[source] = true
			}
			return reportArchiveError(c.GetRef(), e)
		}
		associate(doc, meta)
		doc.SyncRun = run
		return emit(*doc)
	}
	for _, p := range packages {
		if s.pending != nil && !s.pending[p.Locator] {
			continue
		}
		meta := &packagePages{id: idFor(p.Locator), fingerprint: p.Fingerprint, pages: map[string][]*page{}}
		if e := readPackagePages(ctx, p, meta); e != nil {
			failed[p.Locator] = true
			if err := reportArchiveError(p.Locator, e); err != nil {
				return err
			}
			meta.pages = nil
		}
		for _, source := range p.Sources {
			if e := stage.ScanSource(ctx, source, func(c *cdx.Capture) error { return extract(c, meta, p.Locator) }); e != nil {
				return e
			}
		}
		// Only emit unmatched pages once every capture has been associated.
		if failed[p.Locator] {
			continue
		}
		if err = inlineDocuments(meta, run, emit); err != nil {
			return err
		}
	}
	for {
		var c cdx.Capture
		err = json.UnmarshalDecode(dec, &c)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err = extract(&c, nil, c.GetSource()); err != nil {
			return err
		}
	}

	if err = flush(); err != nil {
		return err
	}
	if firstArchiveError != nil {
		s.retryRevision, s.retryRun, s.pending = revision, run, failed
		return fmt.Errorf("could not index all archive text (%d archive errors): %w", archiveErrors, firstArchiveError)
	}
	if err = s.removeOldDocuments(ctx, run); err != nil {
		return err
	}
	if err := s.Index.index.SetInternal([]byte("revision"), []byte(revision)); err != nil {
		return err
	}
	s.pending = nil
	return nil
}

func (s *Synchronizer) removeOldDocuments(ctx context.Context, run string) error {
	// Use repeated first pages: deleting a page shifts later matches forward.
	old := bleve.NewBooleanQuery()
	all := bleve.NewMatchAllQuery()
	current := bleve.NewTermQuery(run)
	current.SetField("syncRun")
	old.AddMust(all)
	old.AddMustNot(current)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, e := s.Index.index.SearchInContext(ctx, bleve.NewSearchRequestOptions(old, 128, 0, false))
		if e != nil {
			return e
		}
		if len(result.Hits) == 0 {
			return nil
		}
		ids := make([]string, 0, len(result.Hits))
		for _, h := range result.Hits {
			ids = append(ids, h.ID)
		}
		if err := s.Index.Publish(ctx, nil, ids); err != nil {
			return err
		}
	}
}

// Visible suppresses obsolete hits while a replacement or deletion is pending.
func (s *Synchronizer) Visible(ctx context.Context, fields map[string]any) bool {
	uri, _ := fields["captureURI"].(string)
	ref, _ := fields["ref"].(string)
	generation, _ := fields["generation"].(string)
	if uri == "" && strings.HasPrefix(ref, "page:") {
		id, _ := fields[packageField].(string)
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
