// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

// Package captures serves writable collections without owning replay access policy.
package captures

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"uuid"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
)

type Options struct {
	Directory, Prefix               string
	ReadOnly                        bool
	MaxBytes                        int64
	MaxLine, ScanLimit, ResultLimit int
	UploadTimeout                   time.Duration
}
type Handler struct {
	Options

	mu     sync.Mutex
	dbs    map[string]*cdxstore.DB
	single *cdxstore.DB
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func New(ctx context.Context, o Options, single *cdxstore.DB) (*Handler, error) {
	if o.MaxBytes == 0 {
		o.MaxBytes = 128 << 20
	}
	if o.MaxLine == 0 {
		o.MaxLine = 1 << 20
	}
	if o.UploadTimeout == 0 {
		o.UploadTimeout = 5 * time.Minute
	}
	if o.ScanLimit == 0 {
		o.ScanLimit = 10000
	}
	if o.ResultLimit == 0 {
		o.ResultLimit = 10000
	}
	h := &Handler{Options: o, single: single, dbs: map[string]*cdxstore.DB{}}
	if single == nil {
		if !o.ReadOnly {
			if err := os.MkdirAll(o.Directory, 0750); err != nil {
				return nil, err
			}
		}
		entries, err := os.ReadDir(o.Directory)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() || !validName.MatchString(e.Name()) {
				continue
			}
			db, err := cdxstore.Open(ctx, cdxstore.Options{
				Format:    "badger",
				Dir:       filepath.Join(o.Directory, e.Name()),
				ReadOnly:  o.ReadOnly,
				IndexOnly: true,
			})
			if err != nil {
				_ = h.Close()
				return nil, fmt.Errorf("collection %s: %w", e.Name(), err)
			}
			h.dbs[e.Name()] = db
		}
	}
	return h, nil
}
func (h *Handler) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var err error
	for name, db := range h.dbs {
		err = errors.Join(err, db.Close())
		delete(h.dbs, name)
	}
	return err
}

//nolint:gocyclo // Explicit native route dispatch and collection creation.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Prefix != "" && !strings.HasPrefix(r.URL.Path, h.Prefix+"/") {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, h.Prefix)
	if path == "/health" || path == "/ready" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	db := h.single
	route := path
	if db == nil {
		if path == "/collections" && r.Method == http.MethodGet {
			h.mu.Lock()
			names := make([]string, 0, len(h.dbs))
			for n := range h.dbs {
				names = append(names, n)
			}
			sort.Strings(names)
			items := make([]map[string]any, 0, len(names))
			for _, n := range names {
				items = append(items, map[string]any{"name": n, "capabilities": h.dbs[n].Capabilities()})
			}
			h.mu.Unlock()
			writeJSON(w, items)
			return
		}
		if !strings.HasPrefix(path, "/collections/") {
			http.NotFound(w, r)
			return
		}
		name, tail, _ := strings.Cut(strings.TrimPrefix(path, "/collections/"), "/")
		if !validName.MatchString(name) {
			http.Error(w, "invalid collection name", http.StatusBadRequest)
			return
		}
		route = "/" + tail
		h.mu.Lock()
		db = h.dbs[name]
		if tail == "" && r.Method == http.MethodPut {
			if h.ReadOnly {
				h.mu.Unlock()
				http.Error(w, "read-only", http.StatusForbidden)
				return
			}
			if db == nil {
				dir := filepath.Join(h.Directory, name)
				err := os.Mkdir(dir, 0750)
				if err == nil {
					defer func() {
						if db == nil {
							_ = os.RemoveAll(dir)
						}
					}()
					db, err = cdxstore.Open(r.Context(), cdxstore.Options{Format: "badger", Dir: dir, IndexOnly: true})
				}
				if err != nil {
					h.mu.Unlock()
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				h.dbs[name] = db
			}
			h.mu.Unlock()
			writeJSON(w, db.Capabilities())
			return
		}
		h.mu.Unlock()
		if db == nil {
			http.NotFound(w, r)
			return
		}
	}
	switch {
	case route == "/cdx" && r.Method == http.MethodGet:
		cdx.CDXHandlerWithLimits(db, h.ScanLimit, h.ResultLimit)(w, r)
	case (route == "/cdx" || route == "/delete") && r.Method == http.MethodPost:
		h.upload(w, r, db, route == "/delete")
	case route == "/export" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Trailer", "X-Export-Error")
		if err := db.Export(r.Context(), w); err != nil {
			w.Header().Set("X-Export-Error", err.Error())
		}
	case route == "/backup" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Trailer", "X-Backup-Error")
		if err := db.Backup(r.Context(), w); err != nil {
			w.Header().Set("X-Backup-Error", err.Error())
		}
	case route == "/packages" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Trailer", "X-Packages-Error")
		encoder := json.NewEncoder(w)
		err := db.ListPackages(r.Context(), func(p cdxstore.PackageInfo) error { return encoder.Encode(p) })
		if err != nil {
			w.Header().Set("X-Packages-Error", err.Error())
		}
	case route == "/stats" && r.Method == http.MethodGet:
		s, err := db.Stats(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s)
	case strings.HasPrefix(route, "/rejections/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(route, "/rejections/")
		if !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(id) {
			http.NotFound(w, r)
			return
		}
		dir := h.reportDir(db)
		name := filepath.Join(dir, id+".jsonl")
		info, err := os.Stat(name)
		if err != nil || time.Since(info.ModTime()) > 24*time.Hour {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		http.ServeFile(w, r, name)
	default:
		switch route {
		case "/cdx", "/delete", "/export", "/backup", "/packages", "/stats":
			http.Error(w, "method not supported", http.StatusMethodNotAllowed)
		default:
			http.NotFound(w, r)
		}
	}
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
func (h *Handler) reportDir(db *cdxstore.DB) string {
	return filepath.Join(db.Directory(), "rejections")
}

var errRequestTooLarge = errors.New("decoded request exceeds size limit")

type limitedReader struct {
	r    io.Reader
	left int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.left == 0 {
		var one [1]byte
		n, e := l.r.Read(one[:])
		if n > 0 {
			return 0, errRequestTooLarge
		}
		return 0, e
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, e := l.r.Read(p)
	l.left -= int64(n)
	return n, e
}
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, db *cdxstore.DB, remove bool) {
	if h.ReadOnly {
		http.Error(w, "read-only", http.StatusForbidden)
		return
	}
	mode := r.URL.Query().Get("badLines")
	if mode != "" && mode != "skip" {
		http.Error(w, "badLines must be skip or omitted", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.UploadTimeout)
	defer cancel()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(h.UploadTimeout))
	defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
	var reader io.Reader = r.Body
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	default:
		http.Error(w, "unsupported content encoding", http.StatusUnsupportedMediaType)
		return
	}
	reader = &limitedReader{reader, h.MaxBytes}
	o := cdxstore.UploadOptions{SkipBad: mode == "skip", Delete: remove, MaxLine: h.MaxLine}
	var report *os.File
	id := ""
	defer func() {
		if report != nil {
			_ = report.Close()
		}
	}()
	o.Reject = func(rejection cdxstore.Rejection) error {
		if report == nil {
			dir := h.reportDir(db)
			if err := os.MkdirAll(dir, 0750); err != nil {
				return err
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, e := range entries {
				info, err := e.Info()
				if err != nil {
					return err
				}
				if time.Since(info.ModTime()) > 24*time.Hour {
					if err = os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
				}
			}
			id = uuid.NewV7().String()
			report, err = os.OpenFile(filepath.Join(dir, id+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
		}
		return json.NewEncoder(report).Encode(rejection)
	}
	result, err := db.Upload(ctx, reader, o)
	if report != nil {
		syncErr := report.Sync()
		if err == nil {
			err = syncErr
		}
		base := strings.TrimSuffix(strings.TrimSuffix(r.URL.Path, "/cdx"), "/delete")
		result.Report = base + "/rejections/" + id
	}
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusBadRequest
		if errors.Is(err, errRequestTooLarge) || errors.Is(err, cdxstore.ErrLineTooLong) {
			status = http.StatusRequestEntityTooLarge
		} else if errors.Is(err, cdxstore.ErrReadOnly) {
			status = http.StatusForbidden
		} else if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusRequestTimeout
		} else if errors.Is(err, syscall.ENOSPC) {
			status = http.StatusInsufficientStorage
		}
		w.WriteHeader(status)
		writeJSON(w, map[string]any{"error": err.Error(), "result": result})
		return
	}
	writeJSON(w, result)
}
