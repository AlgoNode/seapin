// Command seapin is the IPFS cache gateway (PLAN.md).
//
//	seapin serve               answer /ipfs/ requests from the region's bucket
//	seapin ingest              consume references, fetch and store objects (lanes, kubo sidecar)
//	seapin migrate             apply the catalog schema and create the bucket
//	seapin seed <cid> <file>   store a local file under a CID (operator tool; the content policy applies)
//	seapin health [addr]       exit 0 when /health on addr (default LISTEN_ADDR) is 200 (container healthcheck)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"seapin/internal/catalog"
	"seapin/internal/config"
	"seapin/internal/serve"
	"seapin/internal/store"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(ctx, log)
	case "ingest":
		err = runIngest(ctx, log)
	case "migrate":
		err = runMigrate(ctx, log)
	case "seed":
		err = runSeed(ctx, log, os.Args[2:])
	case "health":
		err = runHealth(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
	}
	if err != nil {
		log.Error(os.Args[1]+" failed", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: seapin serve | ingest | migrate | seed <cid> <file> | health [addr] | version")
	os.Exit(2)
}

func runServe(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.New(cfg.S3)
	if err != nil {
		return err
	}
	cat, err := catalog.Open(ctx, cfg.PGDSN)
	if err != nil {
		return err
	}
	defer cat.Close()

	td := &serve.Takedowns{}
	if err := td.Load(ctx, cat.Takedowns, log); err != nil {
		return err
	}
	go td.Refresh(ctx, cat.Takedowns, cfg.TakedownRefresh, log)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	h := serve.New(serve.Options{
		Store:      st,
		Catalog:    cat,
		Takedowns:  td,
		Log:        log,
		Registry:   reg,
		MissMaxAge: cfg.MissMaxAge,

		DemandHeader:     cfg.DemandHeader,
		DemandWait:       cfg.DemandWait,
		DemandMaxWaiters: cfg.DemandMaxWaiters,
	})
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           h.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// No WriteTimeout: a large video to a slow client legitimately takes minutes.
	}
	servers := []*http.Server{srv}
	if cfg.InternalAddr != "" {
		servers = append(servers, &http.Server{
			Addr:              cfg.InternalAddr,
			Handler:           h.InternalRoutes(cfg.InternalToken),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       120 * time.Second,
		})
	}
	log.Info("serving", "addr", cfg.ListenAddr, "internal", cfg.InternalAddr, "version", version)
	return listen(ctx, log, servers...)
}

// listen runs the servers until ctx ends or one fails, then shuts all down.
func listen(ctx context.Context, log *slog.Logger, servers ...*http.Server) error {
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() { errc <- s.ListenAndServe() }()
	}
	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
	return err
}

func runMigrate(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cat, err := catalog.Open(ctx, cfg.PGDSN)
	if err != nil {
		return err
	}
	defer cat.Close()
	if err := cat.Migrate(ctx); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	st, err := store.New(cfg.S3)
	if err != nil {
		return err
	}
	if err := st.EnsureBucket(ctx); err != nil {
		return fmt.Errorf("bucket %q: %w", cfg.S3.Bucket, err)
	}
	log.Info("catalog and bucket ready", "bucket", cfg.S3.Bucket)
	return nil
}

func runHealth(args []string) error {
	addr := config.ListenAddr()
	if len(args) > 0 {
		addr = args[0]
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + addr + "/health")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}
