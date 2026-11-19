// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package fulltext

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/wacz"
)

type page struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Text        string `json:"text"`
	Timestamp   string `json:"ts"`
	source, ref string
	date        time.Time
	used        bool
}
type packagePages struct {
	id, fingerprint string
	pages           map[string][]*page
}

func idFor(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Read page descriptions from each leaf package, retaining exact timestamps.
// Bounds are explicit: malformed or oversized metadata is reported, not ignored.
func readPackagePages(ctx context.Context, p cdxstore.PackageInfo, meta *packagePages) (resultErr error) {
	file, err := filewalker.OpenSource(ctx, p.Locator)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(file, info.Size())
	if err != nil {
		return err
	}
	if p.SourceFingerprint == "" {
		return fmt.Errorf("reindex WACZ before rebuilding page text: %s", p.Locator)
	}
	if wacz.Fingerprint(info, zr) != p.SourceFingerprint {
		return fmt.Errorf("%s: %w", p.Locator, cdxstore.ErrArchiveChanged)
	}
	var root filewalker.Source
	if err = json.Unmarshal([]byte(p.Locator), &root); err != nil {
		return err
	}
	roots := append(append([]string{}, root.Roots...), root.Path)
	var total int64
	for _, member := range zr.File {
		if !strings.HasPrefix(member.Name, "pages/") || !strings.HasSuffix(member.Name, ".jsonl") {
			continue
		}
		if member.UncompressedSize64 > 64<<20 || total+int64(member.UncompressedSize64) > 64<<20 {
			return fmt.Errorf("page metadata exceeds 64 MiB in %s", p.Locator)
		}
		total += int64(member.UncompressedSize64)
		r, e := member.Open()
		if e != nil {
			return e
		}
		locator, _ := json.Marshal(filewalker.Source{Roots: roots, Path: "/" + member.Name})
		scanner := bufio.NewScanner(io.LimitReader(r, 64<<20+1))
		scanner.Buffer(make([]byte, 64<<10), 16<<20)
		line := 0
		for scanner.Scan() {
			line++
			if e = ctx.Err(); e != nil {
				break
			}
			var item page
			if e = json.Unmarshal(scanner.Bytes(), &item); e != nil {
				break
			}
			if item.URL == "" {
				continue
			}
			item.date, e = time.Parse(time.RFC3339Nano, item.Timestamp)
			if e != nil {
				break
			}
			item.source = string(locator)
			item.ref = fmt.Sprintf("page:%s#%d", locator, line)
			meta.pages[item.URL] = append(meta.pages[item.URL], &item)
		}
		if e == nil {
			e = scanner.Err()
		}
		closeErr := r.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
func associate(doc *Document, meta *packagePages) {
	if meta == nil {
		return
	}
	doc.Package = meta.id
	var best *page
	var distance time.Duration
	tie := false
	for _, item := range meta.pages[doc.URL] {
		d := item.date.Sub(doc.Timestamp)
		if d < 0 {
			d = -d
		}
		if best == nil || d < distance {
			best = item
			distance = d
			tie = false
		} else if d == distance {
			tie = true
		}
	}
	if best != nil && !tie {
		doc.Title = best.Title
		doc.PageTimestamp = &best.date
		doc.Inferred = true
		best.used = true
	}
}
func inlineDocuments(meta *packagePages, run string, emit func(Document) error) error {
	for _, pages := range meta.pages {
		for _, p := range pages {
			if p.used || p.Text == "" {
				continue
			}
			u, e := url.Parse(p.URL)
			if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
				continue
			}
			if err := emit(Document{
				ID:            idFor(p.ref),
				URL:           p.URL,
				Host:          strings.ToLower(u.Hostname()),
				Title:         p.Title,
				Text:          p.Text,
				Timestamp:     p.date,
				PageTimestamp: &p.date,
				Package:       meta.id,
				Source:        p.source,
				Ref:           p.ref,
				Generation:    meta.fingerprint,
				SyncRun:       run,
				Snippet:       snippet(p.Text),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
