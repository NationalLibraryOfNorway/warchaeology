// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

// Package archiveapi presents registered archive members as ordinary HTTP WARC
// files. Replay clients never need to know whether a file lives inside a WACZ.
package archiveapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
)

type source struct{ *cdxstore.DB }

func (s source) Available(ctx context.Context, c *cdx.Capture) (bool, error) {
	if c.GetSource() == "" || c.GetRef() == "" {
		return false, nil
	}
	_, _, err := s.ArchiveSource(ctx, cdxstore.ArchiveID(c.GetSource()))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
func New(db *cdxstore.DB, scanLimit, resultLimit int) http.Handler {
	if scanLimit <= 0 {
		scanLimit = 10000
	}
	if resultLimit <= 0 {
		resultLimit = 10000
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /archives/cdx", cdx.CDXHandlerWithTransform(source{db}, scanLimit, resultLimit, func(ctx context.Context, c *cdx.Capture, fields map[string]any) error {
		id := cdxstore.ArchiveID(c.GetSource())
		name := "capture.warc"
		if strings.HasSuffix(c.GetFilename(), ".gz") {
			name += ".gz"
		}
		fields["filename"] = id + "/" + c.GetGeneration() + "/" + name
		return nil
	}))
	mux.HandleFunc("GET /archives/{id}/{generation}/{name}", func(w http.ResponseWriter, r *http.Request) {
		info, generation, err := db.ArchiveSource(r.Context(), r.PathValue("id"))
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "archive catalogue unavailable", 500)
			return
		}
		if generation != r.PathValue("generation") {
			http.Error(w, "archive generation changed; refresh capture index", http.StatusGone)
			return
		}
		file, mounts, err := filewalker.OpenSource(info.GetPath())
		if err != nil {
			http.Error(w, "archive source unavailable", 404)
			return
		}
		defer file.Close()
		defer mounts.Close()
		stat, err := file.Stat()
		if err != nil {
			http.Error(w, "archive metadata unavailable", 500)
			return
		}
		if stat.Size() != info.GetSize() {
			http.Error(w, "archive source changed; reindex before replay", 409)
			return
		}
		// Content-Encoding must remain unset: offsets refer to exact stored WARC
		// bytes, including gzip members. HTTP clients must not decompress transport.
		mime := "application/warc"
		if strings.HasSuffix(info.GetName(), ".gz") {
			mime = "application/gzip"
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("ETag", fmt.Sprintf("\"%s-%s\"", r.PathValue("id"), generation))
		http.ServeContent(w, r, info.GetName(), info.GetLastModified().AsTime(), file)
	})
	return mux
}
