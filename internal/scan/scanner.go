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
	"github.com/algorand/go-algorand-sdk/v2/client/v2/common/models"
	"github.com/algorand/go-algorand-sdk/v2/encoding/msgpack"
	"github.com/algorand/go-algorand-sdk/v2/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/semaphore"

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
	Window         int    // backfill rounds between cursor checkpoints
	Fetchers       int    // backfill block requests in flight
	MemoryBudget   int64  // bytes of decoded blocks the backfill may hold ahead of the one it applies; 0 = no limit
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

// runBackfill walks from next down to floor until it gets there, restarting
// from where it stopped after an error.
func (s *Scanner) runBackfill(ctx context.Context, next, floor int64) {
	s.round.WithLabelValues("backfill").Set(float64(next))
	for ctx.Err() == nil {
		if next < floor {
			s.Log.Info("backfill reached its floor", "floor", floor)
			return
		}
		if err := s.backfill(ctx, &next, floor); err != nil && ctx.Err() == nil {
			s.Log.Error("backfill", "round", next, "err", err)
			sleep(ctx, 5*time.Second)
		}
	}
}

// DecodedPerWireByte is the heap a decoded block takes per byte of its
// msgpack (measured 11.4 on a 3.3 MB, 2,703-transaction mainnet block at round
// 54,486,458; scripts/blockmem), rounded up.
const DecodedPerWireByte = 12

type fetched struct {
	b      types.Block
	err    error
	weight int64 // MemoryBudget held until the block is applied
}

// backfill applies rounds *next down to floor, newest first. Fetching is a
// pipeline: s.Fetchers requests are always in flight and a slot frees up as
// soon as its block arrives, while blocks are applied strictly in round order
// from a look-ahead of 4×s.Fetchers. A slow block delays only its own turn, not
// the fetching behind it. *next advances after each applied block and is
// checkpointed every s.Window rounds; a crash re-applies at most that many,
// which is harmless.
//
// Memory: a block is downloaded raw, and before it is decoded it reserves
// DecodedPerWireByte × its size from s.MemoryBudget, returned once applied.
// Reservations are taken strictly in round order (each waits for the previous
// round's), so the block applied next never waits behind later ones: no
// deadlock, and a run of large blocks (rounds ~54.49 M: 218 MB of msgpack in
// 320 rounds) shortens the look-ahead instead of exhausting the heap. Raw
// blocks waiting for their turn are bounded by the look-ahead (4×s.Fetchers
// blocks, ~175 MB at 64 fetchers in that stretch), so the scanner's live heap
// stays under MemoryBudget plus that.
func (s *Scanner) backfill(ctx context.Context, next *int64, floor int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fetchers := max(1, s.Fetchers)
	slots := make(chan struct{}, fetchers)
	queue := make(chan chan fetched, 4*fetchers)
	var sem *semaphore.Weighted
	if s.MemoryBudget > 0 {
		sem = semaphore.NewWeighted(s.MemoryBudget)
	}
	from := *next
	go func() {
		defer close(queue)
		prev := make(chan struct{})
		close(prev)
		for r := from; r >= floor; r-- {
			res := make(chan fetched, 1)
			select {
			case queue <- res:
			case <-ctx.Done():
				return
			}
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			turn := make(chan struct{})
			go func(r int64, prev, turn chan struct{}) {
				raw, err := s.blockRaw(ctx, s.Blocks, uint64(r))
				<-slots // a slow block must not hold up the downloads behind it
				var w int64
				select {
				case <-prev:
				case <-ctx.Done():
					err = ctx.Err()
				}
				if err == nil && sem != nil {
					w = min(int64(len(raw))*DecodedPerWireByte, s.MemoryBudget)
					if err = sem.Acquire(ctx, w); err != nil {
						w = 0
					}
				}
				close(turn)
				var b types.Block
				if err == nil {
					b, err = s.decodeBlock(raw, uint64(r))
				}
				res <- fetched{b, err, w}
			}(r, prev, turn)
			prev = turn
		}
	}()

	applied, published, start := 0, 0, time.Now()
	checkpoint := func() error {
		if _, err := s.DB.Exec(ctx, `UPDATE scan_state SET backfill_next = $2, updated_at = now() WHERE chain = $1`, s.Chain, *next); err != nil {
			return err
		}
		s.round.WithLabelValues("backfill").Set(float64(*next))
		if s.MaxRefsPerSec > 0 {
			sleep(ctx, time.Duration(published)*time.Second/time.Duration(s.MaxRefsPerSec)-time.Since(start))
		}
		applied, published, start = 0, 0, time.Now()
		return nil
	}
	for res := range queue {
		var f fetched
		select {
		case f = <-res:
		case <-ctx.Done():
			return ctx.Err()
		}
		if f.err != nil {
			return f.err
		}
		for s.liveLag() > s.MaxLiveLag && ctx.Err() == nil {
			sleep(ctx, time.Second)
		}
		n, err := s.processBlockCounted(ctx, f.b, contract.SrcBackfill)
		if err != nil {
			return err
		}
		if f.weight > 0 {
			sem.Release(f.weight)
		}
		*next = int64(f.b.Round) - 1
		published += n
		if applied++; applied >= s.Window {
			if err := checkpoint(); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return checkpoint()
}

// blockRaw fetches one block's msgpack, retrying transient failures.
func (s *Scanner) blockRaw(ctx context.Context, c *algod.Client, round uint64) ([]byte, error) {
	var err error
	for attempt := range 6 {
		var raw []byte
		if raw, err = c.BlockRaw(round).Do(ctx); err == nil {
			return raw, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		sleep(ctx, time.Duration(1<<attempt)*250*time.Millisecond)
	}
	return nil, fmt.Errorf("block %d: %w", round, err)
}

// decodeBlock decodes what blockRaw returned and checks the chain.
func (s *Scanner) decodeBlock(raw []byte, round uint64) (types.Block, error) {
	var resp models.BlockResponse
	if err := msgpack.Decode(raw, &resp); err != nil {
		return types.Block{}, fmt.Errorf("block %d: %w", round, err)
	}
	if resp.Block.GenesisID != s.Chain {
		return resp.Block, fmt.Errorf("round %d belongs to %q, not %q", round, resp.Block.GenesisID, s.Chain)
	}
	return resp.Block, nil
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
