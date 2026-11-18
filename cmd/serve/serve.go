package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"os"

	"github.com/julienschmidt/httprouter"
	indexcmd "github.com/nationallibraryofnorway/warchaeology/v5/cmd/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/archiveapi"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/captures"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/coreserver"
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
		Short:             "Serve a capture database or immutable zipnum cluster",
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
	cmd.AddCommand(&cobra.Command{Use: "restore BACKUP NEW-DIRECTORY", Short: "Restore a capture backup offline into a new directory", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		f, e := os.Open(args[0])
		if e != nil {
			return e
		}
		defer f.Close()
		return cdxstore.Restore(cmd.Context(), args[1], f)
	}})
	cmd.AddCommand(&cobra.Command{Use: "compact DIRECTORY", Short: "Reclaim capture database storage offline", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (err error) {
		if _, e := os.Stat(args[0]); e != nil {
			return e
		}
		db, e := cdxstore.Open(cmd.Context(), cdxstore.Options{Format: badgerFormat, Dir: args[0]})
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, db.Close()) }()
		return db.Compact(cmd.Context())
	}})
	return cmd
}

type ServeOptions struct {
	httpOptions                        captures.Options
	collectionsDir                     string
	address, prefix, format, zipnumDir string
	readOnly, hasInputs                bool
	prefixLimit                        int
	incompatible                       []string
	database                           cdxstore.Options
	ingestion                          *indexcmd.IndexOptions
	closer                             io.Closer
}

func (o *ServeOptions) Complete(cmd *cobra.Command, args []string) error {
	o.hasInputs = o.hasInputs || len(args) > 0
	if o.collectionsDir != "" {
		for _, name := range []string{"badger-dir", "badger-database", "zipnum-dir"} {
			if flag.Explicit(cmd, name) {
				o.incompatible = append(o.incompatible, name)
			}
		}
	}
	if o.format == zipnumFormat {
		for _, name := range []string{"badger-dir", "badger-database", "index-out", "post-append", "input-file"} {
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var handler http.Handler
	if o.collectionsDir != "" {
		opts := o.httpOptions
		opts.Directory = o.collectionsDir
		opts.Prefix = o.prefix
		opts.ReadOnly = o.readOnly
		h, err := captures.New(ctx, opts, nil)
		if err != nil {
			return err
		}
		o.closer = h
		handler = h
	} else if o.format == zipnumFormat {
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
		mux.HandleFunc("GET "+o.prefix+"/warcserver/cdx", cdx.CDXHandlerWithLimits(reader, scanLimit, resultLimit))
		handler = mux
	} else {
		o.database.IndexOnly = !o.hasInputs
		db, err := cdxstore.Open(ctx, o.database)
		if err != nil {
			return err
		}
		o.closer = db
		handler = o.databaseHandler(ctx, db)
		// Browser-origin writes must not cross from archived pages into management.
		previous := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
				origin := r.Header.Get("Origin")
				if origin != "" {
					scheme := "http"
					if r.TLS != nil {
						scheme = "https"
					}
					if origin != scheme+"://"+r.Host {
						http.Error(w, "cross-origin mutation rejected", 403)
						return
					}
				}
				if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
					http.Error(w, "cross-site mutation rejected", 403)
					return
				}
			}
			previous.ServeHTTP(w, r)
		})
		if o.hasInputs {
			o.ingestion.Sink = db
		}
	}
	var ingestion *indexcmd.IndexOptions
	if o.hasInputs {
		ingestion = o.ingestion
	}
	return run(ctx, o.address, handler, ingestion)
}

func (o *ServeOptions) databaseHandler(ctx context.Context, db *cdxstore.DB) http.Handler {
	l := &loader.Loader{StorageRefResolver: db, RecordLoader: loader.FileStorageLoader{}}
	core := coreserver.Handler{Index: db, Loader: l, DebugAPI: db}
	ws := warcserver.Handler{Index: db, Loader: l, Config: &warcserver.Config{PrefixSearchLimit: o.prefixLimit}}
	router := httprouter.New()
	for _, route := range core.Routes() {
		if route.Path == cdxPath || (o.readOnly && route.Method != http.MethodGet) {
			continue
		}
		router.Handler(route.Method, o.prefix+route.Path, route.Handler)
	}
	for _, route := range ws.Routes() {
		if route.Path == cdxPath {
			continue
		}
		router.Handler(route.Method, o.prefix+"/warcserver"+route.Path, route.Handler)
	}
	opts := o.httpOptions
	opts.Prefix = o.prefix
	opts.ReadOnly = o.readOnly
	native, _ := captures.New(ctx, opts, db)
	archives := archiveapi.New(db, native.ScanLimit, native.ResultLimit)
	router.HandlerFunc("GET", o.prefix+"/warcserver/cdx", cdx.CDXHandlerWithLimits(db, native.ScanLimit, native.ResultLimit))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, o.prefix)
		if !db.IndexOnly() && (path == "/pywb/cdx" || strings.HasPrefix(path, "/archives/")) {
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
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, BaseContext: func(net.Listener) context.Context { return ctx }}
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
