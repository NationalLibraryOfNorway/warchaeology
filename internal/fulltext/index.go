// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

// Package fulltext owns the rebuildable text index. Capture locations and their
// visibility remain the responsibility of the capture catalogue.
package fulltext

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	_ "github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/search/query"
)

// Document identifies an extraction occurrence, rather than a URL or digest.
// Callers must derive ID from the complete source locator and record offset.
type Document struct {
	PageTimestamp *time.Time `json:"pageTimestamp,omitempty"`
	Inferred      bool       `json:"inferred"`
	ID            string     `json:"id"`
	URL           string     `json:"url"`
	Host          string     `json:"host"`
	Title         string     `json:"title"`
	Text          string     `json:"text"`
	Timestamp     time.Time  `json:"timestamp"`
	Package       string     `json:"package"`
	Source        string     `json:"source"`
	Generation    string     `json:"generation"`
	CaptureKey    string     `json:"captureKey,omitempty"`
	CaptureURI    string     `json:"captureURI"` //nolint:tagliatelle // Preserve the stored search field name.
	Ref           string     `json:"ref"`
	SyncRun       string     `json:"syncRun"`
	Snippet       string     `json:"snippet"`
}

const packageField = "package"

type Index struct{ index bleve.Index }

func Open(path string) (*Index, error) {
	created := false
	idx, err := bleve.Open(path)
	if errors.Is(err, bleve.ErrorIndexPathDoesNotExist) {
		m := bleve.NewIndexMapping()
		if err := m.AddCustomAnalyzer("archive_text", map[string]any{"type": "custom", "tokenizer": "unicode", "token_filters": []string{"to_lower"}}); err != nil {
			return nil, err
		}
		m.DefaultAnalyzer = "archive_text"
		d := bleve.NewDocumentMapping()
		for _, name := range []string{"id", "url", "host", packageField, "source", "generation", "captureKey", "captureURI", "ref", "syncRun", "snippet"} {
			field := bleve.NewKeywordFieldMapping()
			d.AddFieldMappingsAt(name, field)
		}
		for _, name := range []string{"title", "text"} {
			d.AddFieldMappingsAt(name, bleve.NewTextFieldMapping())
		}
		d.AddFieldMappingsAt("timestamp", bleve.NewDateTimeFieldMapping())
		d.AddFieldMappingsAt("pageTimestamp", bleve.NewDateTimeFieldMapping())
		d.AddFieldMappingsAt("inferred", bleve.NewBooleanFieldMapping())
		m.DefaultMapping = d
		idx, err = bleve.NewUsing(path, m, "scorch", bleve.Config.DefaultKVStore, nil)
		created = err == nil
	}
	if err != nil {
		return nil, fmt.Errorf("open text index: %w", err)
	}
	if created {
		err = idx.SetInternal([]byte("schema"), []byte("1"))
	} else {
		err = checkSchema(idx)
	}
	if err != nil {
		_ = idx.Close()
		return nil, err
	}
	return &Index{index: idx}, nil
}

func (i *Index) Close() error { return i.index.Close() }

// Publish atomically replaces the supplied documents and removes obsolete IDs.
// Keep batches bounded; a source checkpoint is acknowledged by the caller only
// after every batch has succeeded. Retrying IDs is safe.
func (i *Index) Publish(ctx context.Context, docs []Document, removed []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	batch := i.index.NewBatch()
	for _, d := range docs {
		if d.ID == "" || d.URL == "" {
			return errors.New("text document requires ID and URL")
		}
		if err := batch.Index(d.ID, d); err != nil {
			return err
		}
	}
	for _, id := range removed {
		batch.Delete(id)
	}
	return i.index.Batch(batch)
}

type Request struct {
	Query         string
	Host          string
	Package       string
	From, To      time.Time
	Offset, Limit int
}

func (i *Index) Search(ctx context.Context, r Request) (*bleve.SearchResult, error) {
	if r.Query == "" {
		return nil, errors.New("query is required")
	}
	if r.Offset < 0 || r.Offset >= 10000 {
		return nil, errors.New("offset must be between 0 and 9999")
	}
	if r.Limit == 0 {
		r.Limit = 20
	}
	if r.Limit < 1 || r.Limit > 100 || r.Offset+r.Limit > 10000 {
		return nil, errors.New("invalid search limit")
	}
	tokens, err := queryTerms(r.Query)
	if err != nil {
		return nil, err
	}
	parts := make([]query.Query, 0, len(tokens))
	for _, token := range tokens {
		if token.phrase {
			text := bleve.NewMatchPhraseQuery(token.text)
			text.SetField("text")
			title := bleve.NewMatchPhraseQuery(token.text)
			title.SetField("title")
			title.SetBoost(3)
			parts = append(parts, bleve.NewDisjunctionQuery(text, title))
		} else {
			text := bleve.NewMatchQuery(token.text)
			text.SetField("text")
			text.SetOperator(query.MatchQueryOperatorAnd)
			title := bleve.NewMatchQuery(token.text)
			title.SetField("title")
			title.SetBoost(3)
			parts = append(parts, bleve.NewDisjunctionQuery(text, title))
		}
	}
	for field, value := range map[string]string{"host": r.Host, packageField: r.Package} {
		if value != "" {
			q := bleve.NewTermQuery(value)
			q.SetField(field)
			parts = append(parts, q)
		}
	}
	if !r.From.IsZero() || !r.To.IsZero() {
		q := bleve.NewDateRangeQuery(r.From, r.To)
		q.SetField("timestamp")
		parts = append(parts, q)
	}
	req := bleve.NewSearchRequestOptions(bleve.NewConjunctionQuery(parts...), r.Limit, r.Offset, false)
	req.Fields = []string{"id", "url", "title", "timestamp", packageField, "source", "generation", "captureKey", "captureURI", "ref", "snippet", "pageTimestamp", "inferred"}
	req.SortBy([]string{"-_score", "_id"})
	req.Highlight = bleve.NewHighlightWithStyle("html")
	req.Highlight.AddField("text")
	result, err := i.index.SearchInContext(ctx, req)
	if result != nil {
		result.Request = req
		for _, hit := range result.Hits {
			if fragments := hit.Fragments["text"]; len(fragments) > 0 {
				hit.Fields["snippet"] = html.UnescapeString(strings.NewReplacer("<mark>", "", "</mark>", "").Replace(fragments[0]))
			}
			hit.Fragments = nil
		}
	}
	return result, err
}

// Exists distinguishes a missing rebuildable index from an unreadable path.
func Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func OpenReadOnly(path string) (*Index, error) {
	idx, err := bleve.OpenUsing(path, map[string]any{"read_only": true})
	if err != nil {
		return nil, err
	}
	if err = checkSchema(idx); err != nil {
		_ = idx.Close()
		return nil, err
	}
	return &Index{index: idx}, nil
}

type queryTerm struct {
	text   string
	phrase bool
}

func queryTerms(raw string) ([]queryTerm, error) {
	var terms []queryTerm
	for raw = strings.TrimSpace(raw); raw != ""; raw = strings.TrimSpace(raw) {
		if raw[0] == '"' {
			end := strings.IndexByte(raw[1:], '"')
			if end < 0 {
				return nil, errors.New("unterminated quoted phrase")
			}
			text := strings.TrimSpace(raw[1 : end+1])
			if text != "" {
				terms = append(terms, queryTerm{text, true})
			}
			raw = raw[end+2:]
		} else {
			end := strings.IndexAny(raw, " \t\r\n\"")
			if end < 0 {
				end = len(raw)
			}
			terms = append(terms, queryTerm{raw[:end], false})
			raw = raw[end:]
		}
	}
	if len(terms) == 0 {
		return nil, errors.New("query requires words")
	}
	return terms, nil
}

// Timeline computes fixed time buckets over every matching document, rather
// than sampling the current result page. Counts may lag catalogue mutations.
type Timeline struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Counts []uint64  `json:"counts"`
}

func (i *Index) Timeline(ctx context.Context, r Request) (Timeline, error) {
	result, err := i.Search(ctx, Request{Query: r.Query, Host: r.Host, Package: r.Package, From: r.From, To: r.To, Limit: 1})
	if err != nil {
		return Timeline{}, err
	}
	if result.Total == 0 {
		return Timeline{Counts: []uint64{}}, nil
	}
	// Reuse the validated query object from the result's search request.
	q := result.Request.Query
	edge := func(sort string) (time.Time, error) {
		req := bleve.NewSearchRequestOptions(q, 1, 0, false)
		req.Fields = []string{"timestamp"}
		req.SortBy([]string{sort})
		out, e := i.index.SearchInContext(ctx, req)
		if e != nil {
			return time.Time{}, e
		}
		if len(out.Hits) == 0 {
			return time.Time{}, nil
		}
		value, _ := out.Hits[0].Fields["timestamp"].(string)
		return time.Parse(time.RFC3339Nano, value)
	}
	first, err := edge("timestamp")
	if err != nil {
		return Timeline{}, err
	}
	last, err := edge("-timestamp")
	if err != nil {
		return Timeline{}, err
	}
	timeline := Timeline{From: first, To: last, Counts: make([]uint64, 48)}
	if first.Equal(last) {
		timeline.Counts[24] = result.Total
		return timeline, nil
	}
	req := bleve.NewSearchRequestOptions(q, 0, 0, false)
	width := max(last.Sub(first)/48, time.Nanosecond)
	for n := range 48 {
		start := first.Add(time.Duration(n) * width)
		end := first.Add(time.Duration(n+1) * width)
		if n == 47 {
			end = last.Add(time.Nanosecond)
		}
		facet := bleve.NewFacetRequest("timestamp", 1)
		facet.AddDateTimeRange("bucket", start, end)
		req.AddFacet(fmt.Sprint(n), facet)
	}
	out, err := i.index.SearchInContext(ctx, req)
	if err != nil {
		return Timeline{}, err
	}
	for n := range 48 {
		f := out.Facets[fmt.Sprint(n)]
		if f != nil && len(f.DateRanges) > 0 {
			timeline.Counts[n] = uint64(f.DateRanges[0].Count)
		}
	}
	return timeline, nil
}

func ValidateQuery(raw string) error { _, err := queryTerms(raw); return err }

func checkSchema(idx bleve.Index) error {
	version, err := idx.GetInternal([]byte("schema"))
	if err != nil {
		return err
	}
	if string(version) != "1" {
		return errors.New("incompatible text index schema; rebuild into a new --fulltext-dir")
	}
	return nil
}
