// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/julienschmidt/httprouter"
	indexcmd "github.com/nationallibraryofnorway/warchaeology/v5/cmd/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fulltext"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/archiveapi"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/captures"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/coreserver"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/searchui"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/warcserver"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipnum"
	"github.com/spf13/cobra"
)

const zipnumFormat = "zipnum"
const badgerFormat = "badger"
const cdxPath = "/cdx"

func NewCmdServe() *cobra.Command {
	flags := NewServeFlags()
	cmd := &cobra.Command{
		Use:               "serve [FILE/DIR...]",
		Short:             "Serve a Badger index, zipnum cluster, or WACZ",
		ValidArgsFunction: flag.SuffixCompletionFn,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			o, err := flags.ToOptions()
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, o.Close()) }()
			if err = o.Complete(cmd, args); err != nil {
				return err
			}
			if err = o.Validate(); err != nil {
				return err
			}
			cmd.SilenceUsage = true
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer cancel()
			return o.Run(ctx)
		},
	}
	flags.AddFlags(cmd)
	return cmd
}

type ServeOptions struct {
	fulltext, textRebuild              bool
	textDir, playbackURLBase           string
	httpOptions                        captures.Options
	collectionsDir                     string
	address, prefix, format, zipnumDir string
	readOnly, hasInputs                bool
	allowSameOriginReplay              bool
	prefixLimit                        int
	incompatible                       []string
	database                           cdxstore.Options
	ingestion                          *indexcmd.IndexOptions
	closer                             io.Closer
	waczMode, waczPath                 string
}

func (o *ServeOptions) Complete(cmd *cobra.Command, args []string) error {
	if o.waczMode == directWacz {
		return o.completeDirect(cmd, args)
	}
	o.hasInputs = o.hasInputs || len(args) > 0
	if flag.Explicit(cmd, flag.IndexFile) {
		return errors.New("index-file is not valid for serving")
	}
	if o.collectionsDir != "" {
		for _, name := range []string{flag.IndexDir, "badger-database", "zipnum-dir"} {
			if flag.Explicit(cmd, name) {
				o.incompatible = append(o.incompatible, name)
			}
		}
	}
	if o.format == zipnumFormat {
		for _, name := range []string{flag.IndexDir, "badger-database", "post-append", "input-file"} {
			if flag.Explicit(cmd, name) {
				o.incompatible = append(o.incompatible, name)
			}
		}
	}
	if o.ingestion != nil {
		return o.ingestion.Complete(cmd, args)
	}
	return nil
}

func (o *ServeOptions) Validate() error {
	if o.playbackURLBase != "" {
		base, err := url.Parse(o.playbackURLBase)
		if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Hostname() == "" || base.User != nil || strings.ContainsAny(o.playbackURLBase, "?#\\") {
			return errors.New("playback-url-base must be an absolute HTTP(S) collection URL without credentials, query, or fragment")
		}
		o.playbackURLBase = strings.TrimRight(base.String(), "/")
	}
	if o.allowSameOriginReplay && (o.collectionsDir != "" || o.format != badgerFormat) {
		return errors.New("same-origin replay requires a single native Badger database")
	}
	if o.fulltext && (o.collectionsDir != "" || o.format != badgerFormat) {
		return errors.New("fulltext requires a single native Badger database")
	}
	if o.textRebuild && (!o.fulltext || o.readOnly) {
		return errors.New("fulltext-rebuild requires writable fulltext mode")
	}
	if o.httpOptions.MaxBytes < 0 || o.httpOptions.MaxLine < 0 || o.httpOptions.UploadTimeout < 0 || o.httpOptions.ScanLimit < 0 || o.httpOptions.ResultLimit < 0 {
		return fmt.Errorf("HTTP limits must be positive")
	}
	if o.collectionsDir != "" {
		if o.hasInputs || o.format != badgerFormat || len(o.incompatible) > 0 {
			return fmt.Errorf("collections-dir requires badger and rejects input files and single-database flags")
		}
	}
	if o.prefix != "" && !strings.HasPrefix(o.prefix, "/") {
		return fmt.Errorf("path-prefix must start with /")
	}
	if o.waczMode == directWacz {
		return nil
	}
	if o.format == zipnumFormat {
		if o.hasInputs {
			return fmt.Errorf("zipnum serving does not accept ingestion inputs")
		}
		if len(o.incompatible) > 0 {
			return fmt.Errorf("%s is not valid for zipnum serving", o.incompatible[0])
		}
		if o.zipnumDir == "" {
			return fmt.Errorf("zipnum-dir is required")
		}
		return nil
	}
	switch o.format {
	case badgerFormat:
	default:
		return fmt.Errorf("unknown database format %q", o.format)
	}
	if o.readOnly && o.hasInputs {
		return fmt.Errorf("read-only serving rejects ingestion inputs")
	}
	if o.hasInputs {
		return o.ingestion.Validate()
	}
	return nil
}

// Close releases resources after serving and ingestion have stopped.
func (o *ServeOptions) Close() error {
	var err error
	if o.ingestion != nil {
		err = o.ingestion.Close()
		o.ingestion = nil
	}
	if o.closer != nil {
		err = errors.Join(err, o.closer.Close())
		o.closer = nil
	}
	return err
}

func (o *ServeOptions) Run(ctx context.Context) error {
	switch {
	case o.waczMode == directWacz:
		return o.runDirect(ctx)
	case o.collectionsDir != "":
		opts := o.httpOptions
		opts.Directory = o.collectionsDir
		opts.Prefix = o.prefix
		opts.ReadOnly = o.readOnly
		handler, err := captures.New(ctx, opts, nil)
		if err != nil {
			return err
		}
		o.closer = handler
		return run(ctx, o.address, guardBrowserWrites(handler), nil)
	case o.format == zipnumFormat:
		return o.runZipnum(ctx)
	default:
		return o.runNative(ctx)
	}
}

func (o *ServeOptions) runZipnum(ctx context.Context) error {
	reader, err := zipnum.OpenContext(ctx, o.zipnumDir)
	if err != nil {
		return err
	}
	o.closer = reader
	mux := http.NewServeMux()
	for _, path := range []string{"/health", "/ready"} {
		mux.HandleFunc("GET "+o.prefix+path, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) })
	}
	scanLimit, resultLimit := o.httpOptions.ScanLimit, o.httpOptions.ResultLimit
	if scanLimit == 0 {
		scanLimit = 10000
	}
	if resultLimit == 0 {
		resultLimit = 10000
	}
	mux.HandleFunc("GET "+o.prefix+cdxPath, cdx.CDXHandlerWithLimits(reader, scanLimit, resultLimit))
	return run(ctx, o.address, mux, nil)
}

func (o *ServeOptions) runNative(ctx context.Context) (err error) {
	o.database.IndexOnly = !o.hasInputs && !o.fulltext && !o.allowSameOriginReplay
	db, err := cdxstore.Open(ctx, o.database)
	if err != nil {
		return err
	}
	o.closer = db
	if o.allowSameOriginReplay && db.IndexOnly() {
		return errors.New("same-origin replay requires registered native archive sources")
	}
	if o.fulltext && db.IndexOnly() {
		return errors.New("fulltext requires registered native archive sources")
	}

	var ingestion *indexcmd.IndexOptions
	if o.hasInputs {
		ingestion = o.ingestion
		ingestion.Sink = db
		ingestion.Fulltext = false
		ingestion.TextRebuild = false
	}

	handler := o.databaseHandler(ctx, db)
	if o.fulltext {
		dir := o.textDir
		if dir == "" {
			dir = filepath.Join(o.database.Dir, "fulltext")
		}
		open := fulltext.Open
		if o.readOnly {
			open = fulltext.OpenReadOnly
		}
		textIndex, openErr := open(dir)
		if openErr != nil {
			return openErr
		}
		defer func() { err = errors.Join(err, textIndex.Close()) }()
		syncer := &fulltext.Synchronizer{Index: textIndex, DB: db}
		if !o.readOnly {
			workerCtx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				syncText(workerCtx, syncer, o.textRebuild)
			}()
			defer func() { stop(); <-done }()
		}
		handler = o.withSearch(handler, searchui.New(textIndex, syncer, o.playbackURLBase))
	}
	return run(ctx, o.address, guardBrowserWrites(handler), ingestion)
}

func syncText(ctx context.Context, syncer *fulltext.Synchronizer, force bool) {
	const poll = 2 * time.Second
	delay := poll
	for {
		if err := syncer.Sync(ctx, force); err != nil && ctx.Err() == nil {
			slog.Error("Text indexing failed", "error", err)
			delay = min(2*delay, time.Minute)
		} else {
			delay = poll
		}
		force = false
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (o *ServeOptions) withSearch(next, console http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, o.prefix)
		switch {
		case p == "/search" || p == "/search/status":
			http.StripPrefix(o.prefix, console).ServeHTTP(w, r)
		case strings.HasPrefix(p, "/console/"):
			http.StripPrefix(o.prefix+"/console", console).ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// guardBrowserWrites rejects cross-origin browser writes to the management API.
func guardBrowserWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			origin := r.Header.Get("Origin")
			if origin != "" {
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				if origin != scheme+"://"+r.Host {
					http.Error(w, "cross-origin mutation rejected", http.StatusForbidden)
					return
				}
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "cross-site mutation rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (o *ServeOptions) databaseHandler(ctx context.Context, db *cdxstore.DB) http.Handler {
	l := &loader.Loader{
		StorageRefResolver: db,
		TargetCaptures:     db.VisitCapturesByTarget,
		RecordLoader:       loader.FileStorageLoader{ValidateSource: db.ValidateArchiveSource},
	}
	core := coreserver.Handler{Index: db, Loader: l, DebugAPI: db}
	ws := warcserver.Handler{Index: db, Loader: l, Config: &warcserver.Config{PrefixSearchLimit: o.prefixLimit}}
	router := httprouter.New()
	for _, route := range core.Routes() {
		if route.Path == cdxPath || (o.readOnly && route.Method != http.MethodGet) {
			continue
		}
		router.Handler(route.Method, o.prefix+route.Path, route.Handler)
	}
	if o.allowSameOriginReplay {
		for _, route := range ws.Routes() {
			if route.Path != cdxPath {
				router.Handler(route.Method, o.prefix+"/warcserver"+route.Path, route.Handler)
			}
		}
	}
	opts := o.httpOptions
	opts.Prefix = o.prefix
	opts.ReadOnly = o.readOnly
	native, _ := captures.New(ctx, opts, db)
	consoleCDX := cdx.ConsoleCDXHandler(db, native.ScanLimit, native.ResultLimit)
	archives := archiveapi.New(db, native.ScanLimit, native.ResultLimit)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ok := strings.CutPrefix(r.URL.Path, o.prefix)
		if !ok || !strings.HasPrefix(path, "/") {
			http.NotFound(w, r)
			return
		}
		if o.fulltext && path == "/search/cdx" && r.Method == http.MethodGet {
			consoleCDX(w, r)
			return
		}
		if !db.IndexOnly() && strings.HasPrefix(path, "/archives/") {
			http.StripPrefix(o.prefix, archives).ServeHTTP(w, r)
			return
		}
		if db.IndexOnly() || path == cdxPath || path == "/delete" || path == "/export" || path == "/backup" || path == "/stats" || path == "/packages" || path == "/health" || path == "/ready" || strings.HasPrefix(path, "/rejections/") {
			native.ServeHTTP(w, r)
		} else {
			router.ServeHTTP(w, r)
		}
	})
}

func cancellationOnly(err error) bool {
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range e.Unwrap() {
			if !cancellationOnly(cause) {
				return false
			}
		}
		return len(e.Unwrap()) > 0
	case interface{ Unwrap() error }:
		return cancellationOnly(e.Unwrap())
	default:
		return errors.Is(err, context.Canceled)
	}
}

func run(ctx context.Context, address string, handler http.Handler, ingestion *indexcmd.IndexOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, e := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if e != nil {
		return e
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	ingestDone := make(chan error, 1)
	if ingestion != nil {
		go func() { ingestDone <- ingestion.Run(ctx) }()
	} else {
		ingestDone <- nil
	}
	slog.Info("Serving captures", "address", listener.Addr().String())
	var ingestErr, serveErr error
	ingestFinished := false
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case ingestErr = <-ingestDone:
			ingestFinished = true
			ingestDone = nil
			if ingestErr != nil {
				break loop
			}
		case serveErr = <-serveDone:
			serveDone = nil
			break loop
		}
	}
	shutdownRequested := ctx.Err() != nil || serveDone == nil
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer stop()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, srv.Close())
	}
	if serveDone != nil {
		serveErr = <-serveDone
	}
	if !ingestFinished {
		ingestErr = <-ingestDone
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	if shutdownRequested && cancellationOnly(ingestErr) {
		ingestErr = nil
	}
	return errors.Join(serveErr, ingestErr, shutdownErr)
}
