// Command seapin-scan reads Algorand blocks and publishes the IPFS references
// of asset configurations to seapin (PLAN.md §1 Scanner). It replaces
// algoipfssvc. One instance runs per chain.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/common"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"seapin/internal/config"
	"seapin/internal/scan"
)

var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log); err != nil {
		log.Error("seapin-scan failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cfg, err := config.LoadScan()
	if err != nil {
		return err
	}
	live, err := algodClient(cfg.AlgodURL, cfg.AlgodToken, cfg.AlgodReferer)
	if err != nil {
		return err
	}
	blocks, err := algodClient(cfg.BlocksURL, cfg.BlocksToken, cfg.BlocksReferer)
	if err != nil {
		return err
	}
	db, err := pgxpool.New(ctx, cfg.PGDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	livePub := &scan.Publisher{URL: cfg.RMQURL, Name: "seapin-scan live"}
	backfillPub := &scan.Publisher{URL: cfg.RMQURL, Name: "seapin-scan backfill"}
	defer livePub.Close()
	defer backfillPub.Close()
	s := &scan.Scanner{
		Chain: cfg.Chain, Algod: live, Blocks: blocks, DB: db,
		Live: livePub, Backfill: backfillPub, Lookup: lookup(live),
		BackfillRounds: cfg.BackfillRounds, Window: cfg.Window, Fetchers: cfg.Fetchers,
		MaxRefsPerSec: cfg.MaxRefsPerSec, MaxLiveLag: cfg.MaxLiveLag, Log: log,
	}
	if err := s.Migrate(ctx); err != nil {
		return fmt.Errorf("scanner state: %w", err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	s.Register(reg)

	var wg sync.WaitGroup
	if cfg.MetaDSN != "" {
		target, err := pgxpool.New(ctx, cfg.MetaDSN)
		if err != nil {
			return err
		}
		defer target.Close()
		mw := &scan.MetaWriter{DB: db, Target: target, Chain: cfg.Chain, SeapinURL: cfg.SeapinURL,
			Token: cfg.SeapinToken, Client: &http.Client{Timeout: 30 * time.Second}, Log: log}
		mw.Register(reg)
		wg.Go(func() { mw.Run(ctx) })
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		res, code := map[string]any{"postgres": "ok", "live_idle_s": int(s.LiveIdle().Seconds())}, http.StatusOK
		if err := db.Ping(r.Context()); err != nil {
			res["postgres"], code = err.Error(), http.StatusServiceUnavailable
		}
		if s.LiveIdle() > 2*time.Minute {
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(res)
	})
	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener", "err", err)
		}
	}()
	defer srv.Shutdown(context.Background())

	log.Info("seapin-scan", "version", version, "chain", cfg.Chain, "addr", cfg.Addr, "meta", cfg.MetaDSN != "")
	err = s.Run(ctx)
	cancel() // stop the compat writer too, so a failed run is reported
	wg.Wait()
	return err
}

func algodClient(url, token, referer string) (*algod.Client, error) {
	var headers []*common.Header
	if referer != "" {
		headers = append(headers, &common.Header{Key: "Referer", Value: referer})
	}
	return algod.MakeClientWithHeaders(url, token, headers)
}

// lookup reads an asset's creation parameters from algod; 404 means destroyed.
func lookup(c *algod.Client) scan.LookupFunc {
	return func(ctx context.Context, id uint64) (string, string, string, string, bool, error) {
		a, err := c.GetAssetByID(id).Do(ctx)
		var bad common.BadRequest
		if errors.As(err, &bad) || isNotFound(err) {
			return "", "", "", "", false, nil
		}
		if err != nil {
			return "", "", "", "", false, err
		}
		p := a.Params
		return p.Url, p.Name, p.UnitName, p.Creator, true, nil
	}
}

func isNotFound(err error) bool {
	var nf common.NotFound
	return errors.As(err, &nf)
}
