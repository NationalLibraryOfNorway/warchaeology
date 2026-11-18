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

	"github.com/julienschmidt/httprouter"
	indexcmd "github.com/nationallibraryofnorway/warchaeology/v5/cmd/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/coreserver"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/warcserver"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipnum"
	"github.com/spf13/cobra"
)

const zipnumFormat = "zipnum"

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
	return cmd
}

type ServeOptions struct {
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
	case "badger":
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
	var handler http.Handler
	if o.format == zipnumFormat {
		reader, err := zipnum.OpenContext(ctx, o.zipnumDir)
		if err != nil {
			return err
		}
		o.closer = reader
		mux := http.NewServeMux()
		mux.HandleFunc("GET "+o.prefix+"/cdx", cdx.CDXHandler(reader))
		mux.HandleFunc("GET "+o.prefix+"/warcserver/cdx", cdx.CDXHandler(reader))
		handler = mux
	} else {
		db, err := cdxstore.Open(ctx, o.database)
		if err != nil {
			return err
		}
		o.closer = db
		handler = o.databaseHandler(db)
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

func (o *ServeOptions) databaseHandler(db *cdxstore.DB) http.Handler {
	l := &loader.Loader{StorageRefResolver: db, RecordLoader: loader.FileStorageLoader{}}
	core := coreserver.Handler{Index: db, Loader: l, DebugAPI: db}
	ws := warcserver.Handler{Index: db, Loader: l, Config: &warcserver.Config{PrefixSearchLimit: o.prefixLimit}}
	router := httprouter.New()
	for _, route := range core.Routes() {
		if route.Path == "/cdx" || (o.readOnly && route.Method != http.MethodGet) {
			continue
		}
		router.Handler(route.Method, o.prefix+route.Path, route.Handler)
	}
	for _, route := range ws.Routes() {
		if route.Path == "/cdx" {
			continue
		}
		router.Handler(route.Method, o.prefix+"/warcserver"+route.Path, route.Handler)
	}
	router.HandlerFunc("GET", o.prefix+"/cdx", cdx.CDXHandler(db))
	router.HandlerFunc("GET", o.prefix+"/warcserver/cdx", cdx.CDXHandler(db))
	return router
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
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: time.Minute, BaseContext: func(net.Listener) context.Context { return ctx }}
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
