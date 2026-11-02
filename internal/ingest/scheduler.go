// Package ingest turns references into cached objects. The intake records
// them in the catalog and never fetches; one worker pool per lane claims due
// fetches from Postgres and runs them, so a slow fetch only ever holds a slot
// in its own lane (PLAN §1 Ingest scheduling).
package ingest

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"seapin/internal/arcmeta"
	"seapin/internal/catalog"
	"seapin/internal/contract"
	"seapin/internal/fetch"
)

// Lane is one worker pool.
type Lane struct {
	Name        string
	Slots       int
	TTFB        time.Duration // first-byte deadline of a first attempt
	RetryTTFB   time.Duration // and of later attempts
	Fair        bool          // at most one fetch per creator per claim
	CreatorCap  int           // most slots one creator may hold; 0 = no cap
	SlowSources bool          // may use SlowOnly sources (Pinata)
	// SlowSourcesRetryOnly keeps SlowOnly sources out of a fetch's first
	// attempt: in the backfill lane most first misses are dead content, and a
	// second 10 s first-byte wait (rainbow, then Pinata) halved the lane's
	// throughput (2026-11-02). Retries (120 s deadline) still use them.
	SlowSourcesRetryOnly bool
}

func DefaultLanes() []Lane {
	return []Lane{
		{Name: catalog.LaneFast, Slots: 48, TTFB: 10 * time.Second, RetryTTFB: 10 * time.Second, Fair: true, CreatorCap: 4},
		{Name: catalog.LaneSlow, Slots: 16, TTFB: 120 * time.Second, RetryTTFB: 120 * time.Second, Fair: true, CreatorCap: 2, SlowSources: true},
		{Name: catalog.LaneBackfill, Slots: 32, TTFB: 10 * time.Second, RetryTTFB: 120 * time.Second, SlowSources: true, SlowSourcesRetryOnly: true},
		// Read-through for whitelisted referers; the fair-share key is the referring site.
		{Name: catalog.LaneDemand, Slots: 32, TTFB: 10 * time.Second, RetryTTFB: 10 * time.Second, Fair: true, CreatorCap: 8},
	}
}

// retryDelays is the ladder after a failed attempt: one fast attempt, then
// slow ones 5 min, 1 h, 6 h, 24 h, 3 d and 7 d later; then the fetch fails.
var retryDelays = []time.Duration{5 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour, 72 * time.Hour, 168 * time.Hour}

// nextAttempt says where a fetch goes after its attempts-th attempt failed;
// ok is false when the ladder is exhausted. Fast-lane and demand fetches move to slow.
func nextAttempt(lane string, attempts int, now time.Time) (next string, at time.Time, ok bool) {
	if attempts < 1 || attempts > len(retryDelays) {
		return "", time.Time{}, false
	}
	if lane == catalog.LaneFast || lane == catalog.LaneDemand {
		lane = catalog.LaneSlow
	}
	return lane, now.Add(retryDelays[attempts-1]), true
}

type Catalog interface {
	Claim(ctx context.Context, lane string, n int, exclude []string, lease time.Duration, fair bool) ([]catalog.Job, error)
	Complete(ctx context.Context, j catalog.Job, o catalog.Outcome) (int64, error)
	Retry(ctx context.Context, j catalog.Job, lane string, at time.Time, cause string) error
	Fail(ctx context.Context, j catalog.Job, cause string) error
	LaneStats(ctx context.Context) (map[string]catalog.LaneStat, error)
}

type Fetcher interface {
	Fetch(ctx context.Context, root, subpath string, ttfb time.Duration, slow bool) (fetch.Result, error)
}

type Publisher interface {
	Publish(ctx context.Context, ev contract.Event)
}

type Scheduler struct {
	Catalog  Catalog
	Fetcher  Fetcher
	Events   Publisher
	Lanes    []Lane
	Region   string
	Ceiling  time.Duration // a transfer's hard limit; leases outlast it
	MaxLinks int
	Log      *slog.Logger

	kick     map[string]chan struct{}
	fetches  *prometheus.CounterVec
	duration *prometheus.HistogramVec
	bytes    *prometheus.CounterVec
	inflight *prometheus.GaugeVec
	pending  *prometheus.GaugeVec
	oldest   *prometheus.GaugeVec
}

// Register creates the scheduler's metrics; call it once before Run.
func (s *Scheduler) Register(reg prometheus.Registerer) {
	s.kick = map[string]chan struct{}{}
	for _, l := range s.Lanes {
		s.kick[l.Name] = make(chan struct{}, 1)
	}
	s.fetches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "seapin_ingest_fetches_total", Help: "Finished fetch attempts by lane and outcome (cached, blocked, retry, failed).",
	}, []string{"lane", "outcome"})
	s.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "seapin_ingest_fetch_seconds", Help: "Duration of fetch attempts by lane and outcome.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600},
	}, []string{"lane", "outcome"})
	s.bytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "seapin_ingest_stored_bytes_total", Help: "Bytes of objects stored, by source.",
	}, []string{"source"})
	s.inflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "seapin_ingest_inflight", Help: "Fetches running, by lane.",
	}, []string{"lane"})
	s.pending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "seapin_ingest_pending", Help: "Fetches waiting, by lane.",
	}, []string{"lane"})
	s.oldest = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "seapin_ingest_oldest_due_seconds", Help: "How long the oldest due fetch has waited, by lane (the fast lane's is the SLO).",
	}, []string{"lane"})
	reg.MustRegister(s.fetches, s.duration, s.bytes, s.inflight, s.pending, s.oldest)
}

// Kick wakes a lane's dispatcher (new work was queued or a slot freed).
func (s *Scheduler) Kick(lane string) {
	if ch, ok := s.kick[lane]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Run dispatches until ctx ends, then waits for running fetches to stop.
func (s *Scheduler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, l := range s.Lanes {
		wg.Go(func() { s.runLane(ctx, l) })
	}
	wg.Go(func() { s.statsLoop(ctx) })
	wg.Wait()
}

func (s *Scheduler) runLane(ctx context.Context, l Lane) {
	lease := max(l.TTFB, l.RetryTTFB) + s.Ceiling + time.Minute
	var (
		mu         sync.Mutex
		inflight   int
		perCreator = map[string]int{}
		workers    sync.WaitGroup
	)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		mu.Lock()
		free := l.Slots - inflight
		var exclude []string
		if l.CreatorCap > 0 {
			for c, n := range perCreator {
				if n >= l.CreatorCap {
					exclude = append(exclude, c)
				}
			}
		}
		mu.Unlock()

		if free > 0 {
			jobs, err := s.Catalog.Claim(ctx, l.Name, free, exclude, lease, l.Fair)
			if err != nil && ctx.Err() == nil {
				s.Log.Error("claim", "lane", l.Name, "err", err)
			}
			for _, j := range jobs {
				mu.Lock()
				inflight++
				perCreator[j.Creator]++
				s.inflight.WithLabelValues(l.Name).Set(float64(inflight))
				mu.Unlock()
				workers.Go(func() {
					s.process(ctx, l, j)
					mu.Lock()
					inflight--
					if perCreator[j.Creator]--; perCreator[j.Creator] == 0 {
						delete(perCreator, j.Creator)
					}
					s.inflight.WithLabelValues(l.Name).Set(float64(inflight))
					mu.Unlock()
					s.Kick(l.Name)
				})
			}
			if len(jobs) > 0 && len(jobs) == free {
				continue
			}
		}
		select {
		case <-ctx.Done():
			workers.Wait()
			return
		case <-tick.C:
		case <-s.kick[l.Name]:
		}
	}
}

func (s *Scheduler) process(ctx context.Context, l Lane, j catalog.Job) {
	ttfb := l.TTFB
	if j.Attempts > 1 {
		ttfb = l.RetryTTFB
	}
	start := time.Now()
	slow := l.SlowSources && (!l.SlowSourcesRetryOnly || j.Attempts > 1)
	res, err := s.Fetcher.Fetch(ctx, j.Root, j.Subpath, ttfb, slow)
	if ctx.Err() != nil {
		return // shutting down: the lease runs out and the fetch is claimed again
	}
	observe := func(outcome string) {
		s.fetches.WithLabelValues(l.Name, outcome).Inc()
		s.duration.WithLabelValues(l.Name, outcome).Observe(time.Since(start).Seconds())
	}
	log := s.Log.With("lane", l.Name, "root", j.Root, "path", j.Subpath, "attempt", j.Attempts)

	if err != nil {
		cause := truncate(err.Error(), 500)
		next, at, ok := nextAttempt(j.Lane, j.Attempts, time.Now())
		if errors.Is(err, fetch.ErrPermanent) || !ok {
			observe("failed")
			log.Info("fetch failed", "err", cause)
			if err := s.Catalog.Fail(ctx, j, cause); err != nil {
				log.Error("record failure", "err", err)
			}
			s.publish(ctx, contract.Event{Root: j.Root, Subpath: j.Subpath, Status: "failed", Reason: cause})
			return
		}
		observe("retry")
		log.Debug("fetch will be retried", "err", cause, "lane", next, "at", at)
		if err := s.Catalog.Retry(ctx, j, next, at, cause); err != nil {
			log.Error("record retry", "err", err)
		}
		return
	}

	o := catalog.Outcome{Leaf: res.Leaf, Status: res.Status, Mime: res.Mime, Size: res.Size, Reason: res.Reason, Source: res.Source}
	if res.JSON != nil {
		links, err := arcmeta.Links(res.JSON, s.MaxLinks)
		if err != nil {
			log.Debug("JSON object did not parse; no links followed", "err", err)
		}
		for _, l := range links {
			o.Links = append(o.Links, catalog.Link{Path: l.Path, CID: l.CID, Subpath: l.Subpath})
		}
	}
	queued, err := s.Catalog.Complete(ctx, j, o)
	if err != nil {
		// The object may be stored already; when the lease runs out the fetch
		// is claimed again and finds it in the bucket.
		log.Error("record result", "err", err)
		return
	}
	outcome := string(res.Status)
	observe(outcome)
	if res.Status == catalog.StatusCached && res.Source != "local" {
		s.bytes.WithLabelValues(res.Source).Add(float64(res.Size))
	}
	if queued > 0 {
		s.Kick(catalog.LaneFast)
		s.Kick(catalog.LaneBackfill)
	}
	s.publish(ctx, contract.Event{
		Root: j.Root, Subpath: j.Subpath, CID: res.Leaf, Status: outcome,
		Mime: res.Mime, Size: res.Size, Reason: res.Reason, Source: res.Source,
	})
}

func (s *Scheduler) publish(ctx context.Context, ev contract.Event) {
	ev.V, ev.Type, ev.Region, ev.At = contract.Version, contract.TypeObject, s.Region, time.Now().UTC()
	s.Events.Publish(ctx, ev)
}

func (s *Scheduler) statsLoop(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		if stats, err := s.Catalog.LaneStats(ctx); err == nil {
			for lane, st := range stats {
				s.pending.WithLabelValues(lane).Set(float64(st.Pending))
				s.oldest.WithLabelValues(lane).Set(st.OldestDue.Seconds())
			}
		} else if ctx.Err() == nil {
			s.Log.Warn("lane stats", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
