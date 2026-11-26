// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/archiveapi"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/wacz"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const directWacz = "direct"

func (o *ServeOptions) completeDirect(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || !strings.EqualFold(filepath.Ext(args[0]), ".wacz") {
		return errors.New("wacz-index direct requires exactly one local .wacz file")
	}
	// Direct serving uses only its package, the listener, and CDX query options.
	allowed := map[string]bool{
		"wacz-index": true, "address": true, "path-prefix": true, "read-only": true,
		"skip-overflow-scans": true, "query-scan-limit": true, "query-result-limit": true, "help": true,
	}

	o.waczPath = args[0]
	var err error
	cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		if err == nil && !allowed[f.Name] && flag.Explicit(cmd, f.Name) {
			err = fmt.Errorf("%s is not valid with wacz-index direct", f.Name)
		}
	})
	return err
}

func (o *ServeOptions) runDirect(ctx context.Context) error {
	reader, err := wacz.Open(ctx, o.waczPath)
	if err != nil {
		return err
	}
	o.closer = reader
	return run(ctx, o.address, http.StripPrefix(o.prefix, o.directHandler(reader)), nil)
}

func (o *ServeOptions) directHandler(reader *wacz.Reader) http.Handler {
	mux := http.NewServeMux()
	scanLimit, resultLimit := o.httpOptions.ScanLimit, o.httpOptions.ResultLimit
	if scanLimit == 0 {
		scanLimit = 10000
	}
	if resultLimit == 0 {
		resultLimit = 10000
	}
	mux.HandleFunc("GET /cdx", directCDX(cdx.CDXHandlerWithLimits(reader, scanLimit, resultLimit)))
	mux.HandleFunc("GET /archives/cdx", directCDX(cdx.HTTPQueryOptions{ScanLimit: scanLimit, ResultLimit: resultLimit, Transform: func(_ context.Context, c *cdx.Capture, fields map[string]any) error {
		fields["filename"] = archiveapi.VirtualFilename(reader.MemberID(c.GetSource()), reader.Generation(), c.GetSource())
		if filename, ok := fields["orig.filename"].(string); ok && filename != "" && filename != "-" {
			member, err := reader.ResolveMember(filename)
			if err != nil {
				return err
			}
			fields["orig.filename"] = archiveapi.VirtualFilename(reader.MemberID(member), reader.Generation(), member)
		}
		return nil
	}}.Handler(reader)))
	mux.HandleFunc("GET /archives/{id}/{generation}/{name}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		file, err := reader.OpenMember(id)
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "WARC member unavailable", http.StatusInternalServerError)
			return
		}
		defer func() {
			if err := file.Close(); err != nil {
				slog.Warn("Failed to close replay source", "error", err)
			}
		}()
		if r.PathValue("generation") != reader.Generation() {
			http.Error(w, "archive generation changed; refresh capture index", http.StatusGone)
			return
		}
		info, err := file.Stat()
		if err != nil {
			http.Error(w, "WARC member metadata unavailable", http.StatusInternalServerError)
			return
		}
		archiveapi.ServeWARC(w, r, info.Name(), id, reader.Generation(), info.ModTime(), file)
	})
	for _, route := range []string{"GET /health", "GET /ready"} {
		mux.HandleFunc(route, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := reader.Check(); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func directCDX(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Has("page") || q.Has("pageSize") || q.Has("showNumPages") {
			http.Error(w, "block pagination requires zipnum", http.StatusBadRequest)
			return
		}
		next(w, r)
	}
}
