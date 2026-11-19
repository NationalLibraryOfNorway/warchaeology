// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

// Package archiveapi presents registered archive members as ordinary HTTP WARC
// files. Replay clients never need to know whether a file lives inside a WACZ.
package archiveapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	archivefs "github.com/nationallibraryofnorway/warchaeology/v5/internal/fs"
)

type source struct{ *cdxstore.DB }

func (s source) Available(ctx context.Context, c *cdx.Capture) (bool, error) {
	if c.GetSource() == "" || c.GetRef() == "" {
		return false, nil
	}
	_, err := s.ArchiveSource(ctx, cdxstore.ArchiveID(c.GetSource()))
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
		fields["filename"] = VirtualFilename(id, c.GetGeneration(), c.GetFilename())
		return nil
	}))
	mux.HandleFunc("GET /archives/{id}/{generation}/{name}", func(w http.ResponseWriter, r *http.Request) {
		info, err := db.ArchiveSource(r.Context(), r.PathValue("id"))
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "archive catalogue unavailable", http.StatusInternalServerError)
			return
		}
		if info.Generation != r.PathValue("generation") {
			http.Error(w, "archive generation changed; refresh capture index", http.StatusGone)
			return
		}
		if !checkContainer(w, info) {
			return
		}
		file, err := filewalker.OpenSource(r.Context(), info.GetPath())
		if errors.Is(err, archivefs.ErrFTPCredentials) {
			http.Error(w, archivefs.ErrFTPCredentials.Error(), http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			http.Error(w, "archive source unavailable", http.StatusNotFound)
			return
		}
		defer func() {
			if err := file.Close(); err != nil {
				slog.Warn("Failed to close replay source", "error", err)
			}
		}()
		stat, err := file.Stat()
		if err != nil {
			http.Error(w, "archive metadata unavailable", http.StatusInternalServerError)
			return
		}
		version := filewalker.FileVersion{Size: info.GetSize(), Modified: info.GetLastModified().AsTime(), CRC: info.CRC}
		if !version.Matches(filewalker.VersionOf(stat)) {
			http.Error(w, "archive source changed; reindex before replay", http.StatusConflict)
			return
		}
		if info.Container != nil && !checkContainer(w, info) {
			return
		}
		ServeWARC(w, r, info.GetName(), r.PathValue("id"), info.Generation, info.GetLastModified().AsTime(), file)
	})
	return mux
}

func checkContainer(w http.ResponseWriter, info cdxstore.ArchiveInfo) bool {
	current, present, err := filewalker.ContainerVersion(info.GetPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		http.Error(w, "archive source unavailable", http.StatusNotFound)
	case err != nil:
		http.Error(w, "archive metadata unavailable", http.StatusInternalServerError)
	case present && (info.Container == nil || !info.Container.Matches(current)):
		http.Error(w, "archive container needs reindexing before replay", http.StatusConflict)
	default:
		return true
	}
	return false
}

// VirtualFilename identifies a WARC member and its generation without exposing its source path.
func VirtualFilename(id, generation, name string) string {
	suffix := "capture.warc"
	if strings.HasSuffix(name, ".gz") {
		suffix += ".gz"
	}
	return id + "/" + generation + "/" + suffix
}

// ServeWARC implements conditional and range requests against the exact stored bytes.
func ServeWARC(w http.ResponseWriter, r *http.Request, name, id, generation string, modified time.Time, file io.ReadSeeker) {
	mime := "application/warc"
	if strings.HasSuffix(name, ".gz") {
		mime = "application/gzip"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", fmt.Sprintf("\"%s-%s\"", id, generation))
	http.ServeContent(w, r, name, modified, file)
}
