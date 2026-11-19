// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warcserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/nationallibraryofnorway/gowarc/v3"
	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/api"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/handlers"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
	"github.com/nationallibraryofnorway/whatwg-url/url"
)

type Config struct {
	PrefixSearchLimit int
	IsWarcServer      bool
}

type Handler struct {
	Index  index.API
	Loader loader.API
	Config *Config
}

func (h Handler) Routes() []api.Route {
	return []api.Route{
		{Method: http.MethodGet, Path: "/cdx", Handler: h.index},
		{Method: http.MethodGet, Path: "/web/:timestamp/*url", Handler: h.resource},
	}
}

func (h Handler) index(w http.ResponseWriter, r *http.Request) {
	coreAPI, err := parseValues(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// limit the number of results when prefix searching
	if coreAPI.MatchType() == index.MatchTypePrefix &&
		coreAPI.Limit() == 0 {
		coreAPI.SetLimit(h.Config.PrefixSearchLimit)
	}

	if coreAPI.Sort() == index.SortClosest &&
		coreAPI.MatchType() == index.MatchTypeExact {
		coreAPI.SetMatchType(index.MatchTypeVerbatim)
	}

	start := time.Now()
	count := 0
	var bytes int64
	defer func() {
		slog.Debug("Warcserver", "count", count, "bytes", bytes, "duration", time.Since(start))
	}()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	response := make(chan index.CdxResponse)

	if err = h.Index.Search(ctx, coreAPI, response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Search failed", "request", coreAPI, "error", err)
		return
	}

	for res := range response {
		err := res.GetError()
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			slog.Warn("failed result", "error", err)
			continue
		}
		cdx := res.GetCdx()
		pywb := &index.Pywb{Cdx: cdx}

		var b []byte

		switch coreAPI.Output() {
		case api.OutputJson:
			b, err = pywb.MarshalJSON()
		default:
			b, err = pywb.MarshalCdxj()
		}

		if err != nil {
			slog.Warn("failed to marshal result", "error", err)
			return
		}
		n, err := w.Write(b)
		if err != nil {
			slog.Warn("failed to write result", "error", err)
			return
		}
		bytes += int64(n)
		count++
	}
}

// resolveRevisit resolves a revisit record by looking up the closest matching target URI and date
func (h Handler) resolveRevisit(ctx context.Context, targetURI string, closest string) (string, error) {
	closestReq, err := parseClosest(targetURI, closest)
	if err != nil {
		return "", fmt.Errorf("failed to parse WARC-Refers-To-Target-URI: %s: %w", targetURI, err)
	}

	response := make(chan index.CdxResponse)
	err = h.Index.Search(ctx, closestReq, response)
	if err != nil {
		return "", fmt.Errorf("failed to get closest match: %s %s: %w", targetURI, closest, err)
	}

	// we are looking for the record's storage ref
	var ref string

	for res := range response {
		if res.GetError() != nil {
			slog.Warn("Error when iterating response of closest search", "uri", targetURI, "closest", closest, "error", res.GetError())
			continue
		}
		ref = res.GetCdx().GetRef()
		break
	}

	return ref, nil
}

func (h Handler) resource(w http.ResponseWriter, r *http.Request) {
	uri, closest := parseResourceRequest(r)

	closestAPI, err := parseClosest(uri, closest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	slog := slog.With("closest", closest, "url", uri)

	start := time.Now()
	defer func() {
		slog.Debug("Fetched resource", "duration", time.Since(start))
	}()

	ctx, cancelQuery := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancelQuery()

	// query API
	response := make(chan index.CdxResponse)
	err = h.Index.Search(ctx, closestAPI, response)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to search closest", "error", err)
		return
	}

	var res index.CdxResponse
	for res = range response {
		err := res.GetError()
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			slog.Warn("Failed cdx response", "error", res.GetError())
			continue
		}
		if res.GetCdx() == nil {
			http.NotFound(w, r)
			return
		}
		break
	}
	if res == nil {
		http.NotFound(w, r)
		return

	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	ref := res.GetCdx().GetRef()

	// load warc record by storage ref
	var warcRecord gowarc.WarcRecord
	retry := true
	for warcRecord == nil {
		warcRecord, err = h.Loader.RecordByStorageRef(ctx, ref)
		var errResolveRevisit loader.ResolveRevisitError
		if errors.As(err, &errResolveRevisit) && retry {
			var date string
			if date, err = timestamp.To14(errResolveRevisit.Date); err == nil {
				ref, err = h.resolveRevisit(ctx, errResolveRevisit.TargetURI, date)
			}
			retry = false
		}
		if _, ok := errors.AsType[loader.WarcRefersToNotFoundError](err); ok {
			http.Error(w, err.Error(), http.StatusNotFound)
			slog.Error("Failed to load record", "error", err)
			return
		}
		if err != nil {
			if warcRecord != nil {
				_ = warcRecord.Close()
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			slog.Error("Failed to load record", "error", err)
			return
		}
	}
	defer func() {
		if err := warcRecord.Close(); err != nil {
			slog.Warn("Failed to close replay source", "error", err)
		}
	}()

	block, ok := warcRecord.Block().(gowarc.HttpResponseBlock)
	if !ok {
		err := fmt.Errorf("record not renderable: %s", warcRecord)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to load resource", "error", err)
		return
	}

	s := block.HttpStatusCode()

	if !isRedirect(s) {
		raw, err := block.RawBytes()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			slog.Error("Failed to load resource", "error", err)
			return
		}
		err = handlers.RenderHTTP(w, raw)
		if err != nil {
			slog.Warn("Failed to load resource", "error", err)
		}
		return
	}
	// handle redirect
	location := block.HttpHeader().Get("Location")
	if location == "" {
		err := errors.New("empty redirect location")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to load resource", "error", err)
		return
	}

	destination := location
	locUrl, err := url.Parse(location)
	if errors.Is(err, url.ErrMissingSchemeNonRelativeURL) {
		locUrl, err = closestAPI.Url().Parse(location)
		if err != nil {
			err = fmt.Errorf("failed to parse relative location header as URL: %s: %s: %w", warcRecord, location, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			slog.Error("Failed to load resource", "error", err)
			return
		}
		destination = locUrl.String()
	}
	if err != nil {
		err = fmt.Errorf("failed to parse location header as URL: %s: %s: %w", warcRecord, location, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to load resource", "error", err)
		return
	}

	response = make(chan index.CdxResponse)
	err = h.Index.Search(ctx, api.ClosestRequest(closest, destination, locUrl), response)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to find closest redirect", "error", err)
		return
	}

	// fields needed to rewrite the location header
	var sts string
	var loc string

	for res := range response {
		if res.GetError() != nil {
			slog.Warn("Failed cdx response", "error", res.GetError())
			continue
		}
		cdx := res.GetCdx()
		sts = timestamp.TimeTo14(cdx.GetSts().AsTime())
		loc = cdx.GetUri()
		break
	}
	if loc == "" {
		http.NotFound(w, r)
		return
	}
	before, after, ok := strings.Cut(loc, "?")
	params := httprouter.ParamsFromContext(r.Context())
	prefix := strings.TrimSuffix(r.URL.Path, params.ByName("timestamp")+params.ByName("url"))
	path := prefix + sts + "id_/" + before
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	u, err := url.Parse(scheme + "://" + host)
	if err != nil {
		err := fmt.Errorf("failed to construct redirect location: %s: %w", loc, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to load resource", "error", err)
		return
	}
	u.SetPathname(path)
	if ok {
		u.SetSearch(after)
	}
	handlers.RenderRedirect(w, u.String())
}

func isRedirect(code int) bool {
	return code == http.StatusMovedPermanently ||
		code == http.StatusFound || code == http.StatusSeeOther ||
		code == http.StatusTemporaryRedirect ||
		code == http.StatusPermanentRedirect
}
