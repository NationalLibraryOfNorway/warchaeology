// Package searchui exposes search as JSON and a framework-free archive console.
package searchui

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fulltext"
)

//go:embed assets/*
var assets embed.FS

// New owns no index resources; the server must close its index after handlers drain.
func New(index *fulltext.Index, synchronizers ...*fulltext.Synchronizer) http.Handler {
	var syncer *fulltext.Synchronizer
	if len(synchronizers) > 0 {
		syncer = synchronizers[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if syncer == nil {
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": index != nil})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"enabled": index != nil, "indexing": syncer.Status()})
	})
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		if index == nil {
			http.Error(w, "full-text search is not enabled", 404)
			return
		}
		values := r.URL.Query()
		req := fulltext.Request{Query: values.Get("q"), Host: values.Get("host"), Package: values.Get("package")}
		for name, target := range map[string]*int{"offset": &req.Offset, "limit": &req.Limit} {
			if raw := values.Get(name); raw != "" {
				value, err := strconv.Atoi(raw)
				if err != nil {
					http.Error(w, "invalid "+name, 400)
					return
				}
				*target = value
			}
		}
		for name, target := range map[string]*time.Time{"from": &req.From, "to": &req.To} {
			if raw := values.Get(name); raw != "" {
				value, err := time.Parse(time.RFC3339, raw)
				if err != nil {
					http.Error(w, "invalid "+name+": use RFC3339", 400)
					return
				}
				*target = value
			}
		}
		if !req.From.IsZero() && !req.To.IsZero() && req.From.After(req.To) {
			http.Error(w, "from must precede to", 400)
			return
		}
		limit := req.Limit
		if limit == 0 {
			limit = 20
		}
		if req.Offset+limit > 10000 || req.Query == "" || req.Offset < 0 || req.Offset >= 10000 || req.Limit < 0 || req.Limit > 100 {
			http.Error(w, "invalid query or pagination", 400)
			return
		}
		if err := fulltext.ValidateQuery(req.Query); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		result, err := index.Search(r.Context(), req)
		if err != nil {
			http.Error(w, "search unavailable", 503)
			return
		}
		if syncer != nil {
			visible := result.Hits[:0]
			for _, hit := range result.Hits {
				if syncer.Visible(r.Context(), hit.Fields) {
					visible = append(visible, hit)
				}
			}
			result.Hits = visible
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		timeline, err := index.Timeline(r.Context(), req)
		if err != nil {
			http.Error(w, "timeline unavailable", 503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"hits": result.Hits, "totalEstimate": result.Total, "timeline": timeline, "offset": req.Offset, "limit": limit})
	})
	files, _ := fs.Sub(assets, "assets")
	mux.Handle("GET /", http.FileServer(http.FS(files)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; frame-src 'none'; worker-src 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}
