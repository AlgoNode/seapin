package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"seapin/internal/catalog"
	"seapin/internal/contract"
	"seapin/internal/fetch"
)

func TestNextAttemptLadder(t *testing.T) {
	now := time.Unix(0, 0)
	lane, at, ok := nextAttempt(catalog.LaneFast, 1, now)
	if !ok || lane != catalog.LaneSlow || at.Sub(now) != 5*time.Minute {
		t.Errorf("after the fast attempt: %s %v %v", lane, at.Sub(now), ok)
	}
	lane, at, ok = nextAttempt(catalog.LaneSlow, 6, now)
	if !ok || lane != catalog.LaneSlow || at.Sub(now) != 7*24*time.Hour {
		t.Errorf("last slow retry: %s %v %v", lane, at.Sub(now), ok)
	}
	if _, _, ok := nextAttempt(catalog.LaneSlow, 7, now); ok {
		t.Error("ladder should end after 7 attempts")
	}
	if lane, _, _ := nextAttempt(catalog.LaneBackfill, 1, now); lane != catalog.LaneBackfill {
		t.Errorf("backfill retries stay in backfill: %s", lane)
	}
}

type fakeCatalog struct {
	completed []catalog.Outcome
	retried   []string
	failed    []string
}

func (f *fakeCatalog) Claim(context.Context, string, int, []string, time.Duration, bool) ([]catalog.Job, error) {
	return nil, nil
}
func (f *fakeCatalog) Complete(_ context.Context, _ catalog.Job, o catalog.Outcome) (int64, error) {
	f.completed = append(f.completed, o)
	return int64(len(o.Links)), nil
}
func (f *fakeCatalog) Retry(_ context.Context, _ catalog.Job, lane string, _ time.Time, _ string) error {
	f.retried = append(f.retried, lane)
	return nil
}
func (f *fakeCatalog) Fail(_ context.Context, j catalog.Job, _ string) error {
	f.failed = append(f.failed, j.Root)
	return nil
}
func (f *fakeCatalog) LaneStats(context.Context) (map[string]catalog.LaneStat, error) {
	return nil, nil
}

type fakeFetcher struct {
	res  fetch.Result
	err  error
	ttfb time.Duration
	slow bool
}

func (f *fakeFetcher) Fetch(_ context.Context, _, _ string, ttfb time.Duration, slow bool) (fetch.Result, error) {
	f.ttfb, f.slow = ttfb, slow
	return f.res, f.err
}

// TestBackfillSlowSourcesOnRetry: the backfill lane's first attempt skips
// SlowOnly sources (Pinata); retries and the slow lane use them.
func TestBackfillSlowSourcesOnRetry(t *testing.T) {
	lanes := DefaultLanes()
	slowLane, backfill := lanes[1], lanes[2]
	leaf := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	f := &fakeFetcher{res: fetch.Result{Leaf: leaf, Status: catalog.StatusCached, Mime: "image/png", Source: "kubo"}}
	s, _, _ := newScheduler(f)
	for _, c := range []struct {
		lane     Lane
		attempts int
		want     bool
	}{{backfill, 1, false}, {backfill, 2, true}, {slowLane, 1, true}} {
		s.process(context.Background(), c.lane, catalog.Job{Root: leaf, Lane: c.lane.Name, Attempts: c.attempts})
		if f.slow != c.want {
			t.Errorf("%s attempt %d: slow sources %v, want %v", c.lane.Name, c.attempts, f.slow, c.want)
		}
	}
}

type events struct{ got []contract.Event }

func (e *events) Publish(_ context.Context, ev contract.Event) { e.got = append(e.got, ev) }

func newScheduler(f Fetcher) (*Scheduler, *fakeCatalog, *events) {
	cat, ev := &fakeCatalog{}, &events{}
	s := &Scheduler{Catalog: cat, Fetcher: f, Events: ev, Lanes: DefaultLanes(), Region: "eu", MaxLinks: 32,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.Register(prometheus.NewRegistry())
	return s, cat, ev
}

func TestProcessOutcomes(t *testing.T) {
	fast, backfill := DefaultLanes()[0], DefaultLanes()[2]
	ctx := context.Background()
	leaf := "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"

	// A cached JSON object: its links go to the catalog, an event goes out.
	f := &fakeFetcher{res: fetch.Result{Leaf: leaf, Status: catalog.StatusCached, Mime: "application/json",
		JSON: []byte(`{"image":"ipfs://` + leaf + `","x":"https://example.com"}`), Source: "kubo"}}
	s, cat, ev := newScheduler(f)
	s.process(ctx, fast, catalog.Job{Root: leaf, Lane: catalog.LaneFast, Attempts: 1})
	if len(cat.completed) != 1 || len(cat.completed[0].Links) != 1 || cat.completed[0].Links[0].Path != "image" {
		t.Fatalf("completed: %+v", cat.completed)
	}
	if len(ev.got) != 1 || ev.got[0].Status != "cached" || ev.got[0].Region != "eu" || ev.got[0].V != contract.Version {
		t.Errorf("event: %+v", ev.got)
	}

	// A retryable failure moves a fast fetch to slow; a permanent one fails it.
	f = &fakeFetcher{err: errors.New("http 504")}
	s, cat, _ = newScheduler(f)
	s.process(ctx, fast, catalog.Job{Root: "r", Lane: catalog.LaneFast, Attempts: 1})
	if len(cat.retried) != 1 || cat.retried[0] != catalog.LaneSlow || len(cat.failed) != 0 {
		t.Errorf("retryable: retried %v failed %v", cat.retried, cat.failed)
	}
	f.err = fetch.ErrPermanent
	s.process(ctx, fast, catalog.Job{Root: "p", Lane: catalog.LaneFast, Attempts: 1})
	if len(cat.failed) != 1 {
		t.Errorf("permanent: failed %v", cat.failed)
	}
	s.process(ctx, fast, catalog.Job{Root: "x", Lane: catalog.LaneSlow, Attempts: 7})
	if len(cat.failed) != 2 {
		t.Errorf("exhausted ladder: failed %v", cat.failed)
	}

	// Backfill retries get the long first-byte deadline.
	s.process(ctx, backfill, catalog.Job{Root: "b", Lane: catalog.LaneBackfill, Attempts: 2})
	if f.ttfb != backfill.RetryTTFB {
		t.Errorf("backfill retry TTFB %v, want %v", f.ttfb, backfill.RetryTTFB)
	}
}
