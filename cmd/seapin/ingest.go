package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"seapin/internal/catalog"
	"seapin/internal/config"
	"seapin/internal/fetch"
	"seapin/internal/ingest"
	"seapin/internal/store"
)

// runIngest consumes the region's queue and runs the fetch lanes (PLAN §1).
func runIngest(ctx context.Context, log *slog.Logger) error {
	cfg, ing, err := config.LoadIngest()
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

	var sources []fetch.Source
	if ing.PeerURL != "" {
		sources = append(sources, fetch.Source{
			Name: "peer", BaseURL: ing.PeerURL + "/internal", MissOK: true,
			Header: http.Header{"Authorization": {"Bearer " + ing.PeerToken}},
		})
	}
	if ing.LegacyURL != "" {
		// The old gateway's caches hold most historical content the network
		// no longer serves (M0 spike); only-if-cached keeps a miss cheap.
		sources = append(sources, fetch.Source{
			Name: "legacy", BaseURL: ing.LegacyURL, MissOK: true,
			Header: http.Header{"Cache-Control": {"only-if-cached"}},
		})
	}
	sources = append(sources, fetch.Source{Name: "sidecar", BaseURL: ing.SidecarURL})
	if ing.PinataURL != "" {
		sources = append(sources, fetch.Source{
			Name: "pinata", BaseURL: ing.PinataURL, SlowOnly: true,
			Header: http.Header{"X-Pinata-Gateway-Token": {ing.PinataToken}},
		})
	}
	fetcher := &fetch.Fetcher{Sources: sources, Client: fetch.NewClient(), Store: st, Limits: fetch.DefaultLimits()}

	lanes := ingest.DefaultLanes()
	for i := range lanes {
		switch lanes[i].Name {
		case catalog.LaneFast:
			lanes[i].Slots, lanes[i].TTFB, lanes[i].RetryTTFB = ing.FastSlots, ing.FastTTFB, ing.FastTTFB
		case catalog.LaneSlow:
			lanes[i].Slots, lanes[i].TTFB, lanes[i].RetryTTFB = ing.SlowSlots, ing.SlowTTFB, ing.SlowTTFB
		case catalog.LaneBackfill:
			lanes[i].Slots, lanes[i].TTFB, lanes[i].RetryTTFB = ing.BackfillSlots, ing.BackfillTTFB, ing.RetryTTFB
		case catalog.LaneDemand:
			lanes[i].Slots, lanes[i].TTFB, lanes[i].RetryTTFB = ing.DemandSlots, ing.DemandTTFB, ing.DemandTTFB
		}
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	events := &ingest.Events{URL: ing.RMQURL, Log: log}
	defer events.Close()
	sched := &ingest.Scheduler{
		Catalog: cat, Fetcher: fetcher, Events: events, Lanes: lanes, Region: ing.Region,
		Ceiling: fetcher.Limits.Ceiling, MaxLinks: 32, Log: log,
	}
	sched.Register(reg)
	intake := &ingest.Intake{
		URL: ing.RMQURL, Queue: "seapin." + ing.Region, QueueType: ing.QueueType,
		MaxLength: ing.QueueMaxLen, Prefetch: ing.Prefetch, Catalog: cat, Queued: sched.Kick, Log: log,
	}
	intake.Register(reg)

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		hctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		res, code := map[string]string{"s3": "ok", "postgres": "ok"}, http.StatusOK
		if err := st.Ping(hctx); err != nil {
			res["s3"], code = err.Error(), http.StatusServiceUnavailable
		}
		if err := cat.Ping(hctx); err != nil {
			res["postgres"], code = err.Error(), http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(res)
	})
	srv := &http.Server{Addr: ing.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	log.Info("ingesting", "region", ing.Region, "sources", len(sources), "addr", ing.Addr, "version", version)
	var wg sync.WaitGroup
	wg.Go(func() { sched.Run(ctx) })
	wg.Go(func() { intake.Run(ctx) })
	wg.Go(func() { // serve's read-through queues fetches and NOTIFYs; wake the lane at once
		for ctx.Err() == nil {
			if err := cat.Notifications(ctx, sched.Kick); err != nil && ctx.Err() == nil {
				log.Warn("fetch notifications, reconnecting", "err", err)
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
				}
			}
		}
	})
	err = listen(ctx, log, srv)
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
