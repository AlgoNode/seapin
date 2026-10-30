package scan

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"seapin/internal/contract"
	"seapin/internal/pgmigrate"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Scanner struct {
	Chain    string        // the chain's genesis id, e.g. mainnet-v1.0; blocks from another chain are refused
	Algod    *algod.Client // live blocks and asset lookups
	Blocks   *algod.Client // backfill blocks (blksrv or any archival algod)
	DB       *pgxpool.Pool // scanner state
	Live     RefPublisher
	Backfill RefPublisher
	Lookup   LookupFunc

	BackfillRounds uint64 // how far below the bootstrap round to go; 0 = to genesis
	Window         int    // rounds fetched in parallel per backfill step
	Fetchers       int
	MaxRefsPerSec  int    // backfill publish budget
	MaxLiveLag     uint64 // backfill pauses while live is further behind the tip
	Log            *slog.Logger

	tip, liveNext atomic.Uint64
	round         *prometheus.GaugeVec
	blocks        *prometheus.CounterVec
	refs          *prometheus.CounterVec
	lag           prometheus.Gauge
	lastLive      atomic.Int64 // unix seconds of the last live progress
}

func (s *Scanner) Migrate(ctx context.Context) error {
	return pgmigrate.Run(ctx, s.DB, migrations, "migrations/*.sql", "scan_schema_migrations", 0x5ea_0002)
}

func (s *Scanner) Register(reg prometheus.Registerer) {
	s.round = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "seapin_scan_next_round", Help: "Next round each cursor reads.",
	}, []string{"cursor"})
	s.blocks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "seapin_scan_blocks_total", Help: "Blocks processed by cursor.",
	}, []string{"cursor"})
	s.refs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "seapin_scan_refs_total", Help: "References published by cursor.",
	}, []string{"cursor"})
	s.lag = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "seapin_scan_live_lag_rounds", Help: "How far the live cursor is behind the node's last round.",
	})
	reg.MustRegister(s.round, s.blocks, s.refs, s.lag)
}

// LiveIdle is how long the live cursor has not moved.
func (s *Scanner) LiveIdle() time.Duration {
	return time.Since(time.Unix(s.lastLive.Load(), 0))
}

// RefPublisher sends refs and returns once the broker has them (*Publisher).
type RefPublisher interface {
	Publish(ctx context.Context, refs []contract.Ref) error
}

type state struct {
	liveNext, backfillNext, floor int64
}

// Run bootstraps the cursors on the first start and runs both until ctx ends.
func (s *Scanner) Run(ctx context.Context) error {
	st, err := s.bootstrap(ctx)
	if err != nil {
		return err
	}
	s.Log.Info("scanning", "chain", s.Chain, "live_next", st.liveNext, "backfill_next", st.backfillNext, "floor", st.floor)
	s.lastLive.Store(time.Now().Unix())
	var wg sync.WaitGroup
	wg.Go(func() { s.runLive(ctx, uint64(st.liveNext)) })
	wg.Go(func() { s.runBackfill(ctx, st.backfillNext, st.floor) })
	wg.Wait()
	return nil
}

func (s *Scanner) bootstrap(ctx context.Context) (state, error) {
	var st state
	err := s.DB.QueryRow(ctx, `SELECT live_next, backfill_next, floor FROM scan_state WHERE chain = $1`, s.Chain).
		Scan(&st.liveNext, &st.backfillNext, &st.floor)
	if err == nil {
		return st, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return st, err
	}
	var last uint64
	for wait := time.Second; ; wait = min(2*wait, 5*time.Minute) {
		status, err := s.Algod.Status().Do(ctx)
		if err == nil {
			last = status.LastRound
			break
		}
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		s.Log.Warn("bootstrap: algod status failed, retrying", "err", err, "in", wait)
		sleep(ctx, wait)
	}
	t0 := int64(last)
	floor := int64(0)
	if s.BackfillRounds > 0 && uint64(t0) > s.BackfillRounds {
		floor = t0 - int64(s.BackfillRounds)
	}
	if _, err := s.DB.Exec(ctx, `
		INSERT INTO scan_state (chain, bootstrap, live_next, backfill_next, floor)
		VALUES ($1, $2, $2, $3, $4) ON CONFLICT (chain) DO NOTHING`, s.Chain, t0, t0-1, floor); err != nil {
		return st, err
	}
	s.Log.Info("bootstrapped at the tip", "round", t0)
	return s.bootstrap(ctx)
}

func (s *Scanner) runLive(ctx context.Context, next uint64) {
	s.liveNext.Store(next)
	wait := time.Second
	for ctx.Err() == nil {
		status, err := s.Algod.StatusAfterBlock(next - 1).Do(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.Log.Warn("live: waiting for the next block", "round", next, "err", err)
				sleep(ctx, wait)
				wait = min(2*wait, 30*time.Second)
			}
			continue
		}
		wait = time.Second
		s.tip.Store(status.LastRound)
		for next <= status.LastRound && ctx.Err() == nil {
			b, err := s.block(ctx, s.Algod, next)
			if err == nil {
				err = s.processBlock(ctx, b, contract.SrcLive)
			}
			if err == nil {
				_, err = s.DB.Exec(ctx, `UPDATE scan_state SET live_next = $2, updated_at = now() WHERE chain = $1`, s.Chain, int64(next+1))
			}
			if err != nil {
				if ctx.Err() == nil {
					s.Log.Error("live", "round", next, "err", err)
					sleep(ctx, 5*time.Second)
				}
				break
			}
			next++
			s.liveNext.Store(next)
			s.lastLive.Store(time.Now().Unix())
			s.round.WithLabelValues("live").Set(float64(next))
			s.lag.Set(float64(s.liveLag()))
		}
	}
}

func (s *Scanner) liveLag() uint64 {
	tip, next := s.tip.Load(), s.liveNext.Load()
	if tip+1 <= next {
		return 0
	}
	return tip + 1 - next
}

// runBackfill walks from next down to floor in windows fetched in parallel,
// processing each window newest first.
func (s *Scanner) runBackfill(ctx context.Context, next, floor int64) {
	s.round.WithLabelValues("backfill").Set(float64(next))
	for ctx.Err() == nil {
		if next < floor {
			s.Log.Info("backfill reached its floor", "floor", floor)
			return
		}
		if s.liveLag() > s.MaxLiveLag {
			sleep(ctx, time.Second)
			continue
		}
		hi, lo := next, max(floor, next-int64(s.Window)+1)
		start := time.Now()
		blocks, err := s.window(ctx, uint64(lo), uint64(hi))
		published := 0
		for i := len(blocks) - 1; err == nil && i >= 0; i-- {
			var n int
			n, err = s.processBlockCounted(ctx, blocks[i], contract.SrcBackfill)
			published += n
		}
		if err == nil {
			_, err = s.DB.Exec(ctx, `UPDATE scan_state SET backfill_next = $2, updated_at = now() WHERE chain = $1`, s.Chain, lo-1)
		}
		if err != nil {
			if ctx.Err() == nil {
				s.Log.Error("backfill", "rounds", fmt.Sprintf("%d-%d", lo, hi), "err", err)
				sleep(ctx, 5*time.Second)
			}
			continue
		}
		next = lo - 1
		s.round.WithLabelValues("backfill").Set(float64(next))
		if s.MaxRefsPerSec > 0 {
			sleep(ctx, time.Duration(published)*time.Second/time.Duration(s.MaxRefsPerSec)-time.Since(start))
		}
	}
}

// window fetches rounds lo..hi with s.Fetchers in parallel, in round order.
func (s *Scanner) window(ctx context.Context, lo, hi uint64) ([]types.Block, error) {
	out := make([]types.Block, hi-lo+1)
	errs := make([]error, len(out))
	sem := make(chan struct{}, max(1, s.Fetchers))
	var wg sync.WaitGroup
	for i := range out {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			out[i], errs[i] = s.block(ctx, s.Blocks, lo+uint64(i))
		})
	}
	wg.Wait()
	return out, errors.Join(errs...)
}

// block fetches one block, retrying transient failures (rate limits, restarts).
func (s *Scanner) block(ctx context.Context, c *algod.Client, round uint64) (types.Block, error) {
	var err error
	for attempt := range 6 {
		var b types.Block
		if b, err = c.Block(round).Do(ctx); err == nil {
			if b.GenesisID != s.Chain {
				return b, fmt.Errorf("round %d belongs to %q, not %q", round, b.GenesisID, s.Chain)
			}
			return b, nil
		}
		if ctx.Err() != nil {
			return b, ctx.Err()
		}
		sleep(ctx, time.Duration(1<<attempt)*250*time.Millisecond)
	}
	return types.Block{}, fmt.Errorf("block %d: %w", round, err)
}

func (s *Scanner) processBlock(ctx context.Context, b types.Block, src string) error {
	_, err := s.processBlockCounted(ctx, b, src)
	return err
}

// processBlockCounted applies a block's asset configurations and publishes
// the resulting refs in one database transaction: the asset rows only change
// once the broker has confirmed the refs. The backfill reads a block's
// configurations newest first, like the blocks themselves.
func (s *Scanner) processBlockCounted(ctx context.Context, b types.Block, src string) (int, error) {
	cursor := src
	s.blocks.WithLabelValues(cursor).Inc()
	evs := Events(b)
	if len(evs) == 0 {
		return 0, nil
	}
	if src == contract.SrcBackfill {
		slices.Reverse(evs)
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var refs []contract.Ref
	for _, ev := range evs {
		r, err := s.apply(ctx, tx, ev, src)
		if err != nil {
			return 0, fmt.Errorf("asset %d at round %d: %w", ev.Asset, ev.Round, err)
		}
		refs = append(refs, r...)
	}
	pub := s.Live
	if src == contract.SrcBackfill {
		pub = s.Backfill
	}
	if err := pub.Publish(ctx, refs); err != nil {
		return 0, fmt.Errorf("publish: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	s.refs.WithLabelValues(cursor).Add(float64(len(refs)))
	return len(refs), nil
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
