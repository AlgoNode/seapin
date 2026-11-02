package scan

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/common/models"
	"github.com/algorand/go-algorand-sdk/v2/encoding/msgpack"
	"github.com/algorand/go-algorand-sdk/v2/types"

	"seapin/internal/contract"
)

// fakeAlgod serves /v2/status, /v2/status/wait-for-block-after and msgpack
// /v2/blocks for rounds up to tip; block r creates asset r with an IPFS URL
// whose CID is derived from r.
type fakeAlgod struct {
	tip     atomic.Uint64
	mu      sync.Mutex
	fetched map[uint64]int
	delay   func(round uint64) time.Duration // per-block latency; nil = none
	empty   bool                             // blocks without transactions
}

const rawCID = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"

func (f *fakeAlgod) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/v2/status" || strings.HasPrefix(path, "/v2/status/wait-for-block-after/"):
		if after, ok := strings.CutPrefix(path, "/v2/status/wait-for-block-after/"); ok {
			if n, _ := strconv.ParseUint(after, 10, 64); n >= f.tip.Load() {
				select { // nothing newer: hold like algod does
				case <-time.After(2 * time.Second):
				case <-r.Context().Done():
				}
			}
		}
		json.NewEncoder(w).Encode(models.NodeStatus{LastRound: f.tip.Load()})
	case strings.HasPrefix(path, "/v2/blocks/"):
		round, _ := strconv.ParseUint(strings.TrimPrefix(path, "/v2/blocks/"), 10, 64)
		if round > f.tip.Load() {
			http.Error(w, `{"message":"no such round"}`, http.StatusNotFound)
			return
		}
		f.mu.Lock()
		f.fetched[round]++
		f.mu.Unlock()
		if f.delay != nil {
			time.Sleep(f.delay(round))
		}
		create := acfg(address(byte(round)), 0, types.AssetParams{URL: "ipfs://" + rawCID + "/" + strconv.FormatUint(round, 10) + ".json"}, "")
		create.ApplyData.ConfigAsset = types.AssetIndex(round)
		b := types.Block{BlockHeader: types.BlockHeader{Round: types.Round(round), TimeStamp: 1_700_000_000, GenesisID: "mainnet-v1.0"}}
		if !f.empty {
			b.Payset = []types.SignedTxnInBlock{{SignedTxnWithAD: create, HasGenesisID: true}}
		}
		w.Header().Set("Content-Type", "application/msgpack")
		w.Write(msgpack.Encode(models.BlockResponse{Block: b}))
	default:
		http.NotFound(w, r)
	}
}

type memPublisher struct {
	mu   sync.Mutex
	refs []contract.Ref
}

func (m *memPublisher) Publish(_ context.Context, refs []contract.Ref) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs = append(m.refs, refs...)
	return nil
}

func (m *memPublisher) rounds() []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []uint64
	for _, r := range m.refs {
		out = append(out, r.Round)
	}
	return out
}

// TestRunCursors bootstraps at the tip, follows new blocks live and walks the
// backfill down to its floor, newest first, with every round seen once.
func TestRunCursors(t *testing.T) {
	s := openScanner(t)
	fa := &fakeAlgod{fetched: map[uint64]int{}}
	fa.tip.Store(1000)
	srv := httptest.NewServer(fa)
	defer srv.Close()
	c, err := algod.MakeClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	live, backfill := &memPublisher{}, &memPublisher{}
	s.Algod, s.Blocks, s.Live, s.Backfill = c, c, live, backfill
	s.BackfillRounds, s.Window, s.Fetchers, s.MaxLiveLag = 25, 8, 4, 10

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Register(newRegistry())
	go func() {
		if err := s.Run(ctx); err != nil {
			t.Error("run:", err)
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for len(backfill.rounds()) < 25 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	got := backfill.rounds()
	if len(got) != 25 || got[0] != 999 || got[24] != 975 {
		t.Fatalf("backfill rounds %v, want 999 down to 975", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] >= got[i-1] {
			t.Fatalf("backfill not newest first: %v", got)
		}
	}
	for len(live.rounds()) < 1 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if lr := live.rounds(); len(lr) != 1 || lr[0] != 1000 {
		t.Fatalf("live rounds %v, want [1000]", lr)
	}
	fa.tip.Store(1002) // two new blocks
	for len(live.rounds()) < 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if lr := live.rounds(); fmt.Sprint(lr) != "[1000 1001 1002]" {
		t.Fatalf("live rounds after new blocks %v", lr)
	}
	cancel()

	var liveNext, backfillNext, floor int64
	s.DB.QueryRow(context.Background(), `SELECT live_next, backfill_next, floor FROM scan_state`).Scan(&liveNext, &backfillNext, &floor)
	if liveNext != 1003 || backfillNext != 974 || floor != 975 {
		t.Errorf("cursors live_next %d backfill_next %d floor %d", liveNext, backfillNext, floor)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	for r, n := range fa.fetched {
		if n != 1 {
			t.Errorf("round %d fetched %d times", r, n)
		}
	}
	ref := live.refs[0]
	if ref.Key != "mainnet-v1.0/1000/url" || ref.Subpath != "1000.json" || ref.Src != contract.SrcLive || ref.TxID == "" {
		t.Errorf("live ref %+v", ref)
	}
}

// TestBackfillPipelinesSlowBlocks: every 10th block takes 300 ms. Fetched in
// windows of 64 with a barrier per window that is at least 7 × 300 ms; the
// pipeline keeps fetching past a slow block, so 400 rounds take well under that.
func TestBackfillPipelinesSlowBlocks(t *testing.T) {
	s := openScanner(t)
	fa := &fakeAlgod{fetched: map[uint64]int{}, empty: true, delay: func(r uint64) time.Duration {
		if r%10 == 0 {
			return 300 * time.Millisecond
		}
		return 5 * time.Millisecond
	}}
	fa.tip.Store(5000)
	srv := httptest.NewServer(fa)
	defer srv.Close()
	c, err := algod.MakeClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Algod, s.Blocks, s.Live, s.Backfill = c, c, &memPublisher{}, &memPublisher{}
	s.Fetchers, s.Window, s.MaxLiveLag = 32, 64, 10
	s.Register(newRegistry())

	next, floor := int64(4999), int64(4600)
	start := time.Now()
	if err := s.backfill(context.Background(), &next, floor); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if next != floor-1 || took > 1500*time.Millisecond {
		t.Fatalf("next %d (want %d), took %v (window barrier >= 2.1 s)", next, floor-1, took)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.fetched) != 400 {
		t.Errorf("fetched %d distinct rounds, want 400", len(fa.fetched))
	}
	t.Logf("400 rounds, every 10th taking 300 ms, 32 fetchers: %v", took.Round(time.Millisecond))
}

// TestBackfillMemoryBudget runs the backfill with a budget below one block
// (each reservation is capped at the budget) and with room for about three
// blocks: it must neither deadlock nor reorder, with slow blocks mixed in.
func TestBackfillMemoryBudget(t *testing.T) {
	for _, budget := range []int64{1, 3 * 400 * DecodedPerWireByte} {
		s := openScanner(t)
		fa := &fakeAlgod{fetched: map[uint64]int{}, delay: func(r uint64) time.Duration {
			if r%7 == 0 {
				return 50 * time.Millisecond
			}
			return time.Millisecond
		}}
		fa.tip.Store(3000)
		srv := httptest.NewServer(fa)
		c, err := algod.MakeClient(srv.URL, "")
		if err != nil {
			t.Fatal(err)
		}
		pub := &memPublisher{}
		s.Algod, s.Blocks, s.Live, s.Backfill = c, c, &memPublisher{}, pub
		s.Fetchers, s.Window, s.MaxLiveLag, s.MemoryBudget = 16, 32, 10, budget
		s.Register(newRegistry())

		next, floor := int64(2999), int64(2800)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err = s.backfill(ctx, &next, floor)
		cancel()
		srv.Close()
		if err != nil || next != floor-1 {
			t.Fatalf("budget %d: next %d (want %d), err %v", budget, next, floor-1, err)
		}
		got := pub.rounds()
		if len(got) != 200 || !slices.IsSortedFunc(got, func(a, b uint64) int { return cmp.Compare(b, a) }) {
			t.Fatalf("budget %d: %d refs, newest first: %v", budget, len(got), slices.IsSortedFunc(got, func(a, b uint64) int { return cmp.Compare(b, a) }))
		}
	}
}

// TestReresolve publishes refs for live assets whose URL the current parser
// resolves and that never produced one, once.
func TestReresolve(t *testing.T) {
	s := openScanner(t)
	fa := &fakeAlgod{fetched: map[uint64]int{}, empty: true}
	fa.tip.Store(100)
	srv := httptest.NewServer(fa)
	defer srv.Close()
	c, err := algod.MakeClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	pub := &memPublisher{}
	s.Algod, s.Blocks, s.Live, s.Backfill = c, c, &memPublisher{}, pub
	s.Register(newRegistry())
	ctx := context.Background()
	for _, row := range []struct {
		asset     int64
		url       string
		destroyed bool
		refRound  any
	}{
		{1, "gateway.pinata.cloud/ipfs/" + rawCID, false, nil}, // scheme-less: resolved now
		{2, "https://example.com/1.json", false, nil},          // not IPFS
		{3, "gateway.pinata.cloud/ipfs/" + rawCID, true, nil},  // destroyed
		{4, "ipfs://" + rawCID, false, int64(50)},              // has refs already
	} {
		if _, err := s.DB.Exec(ctx, `INSERT INTO scan_assets (chain, asset, newest_seq, destroyed, url, creator, ref_round)
			VALUES ('mainnet-v1.0', $1, $2, $3, $4, 'C', $5)`, row.asset, int64(42)<<20, row.destroyed, row.url, row.refRound); err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.Reresolve(ctx, false)
	if err != nil || st.Scanned != 2 || st.Resolved != 1 || st.Published != 1 {
		t.Fatalf("first pass: %+v %v", st, err)
	}
	r := pub.refs[0]
	if r.Asset != 1 || r.Path != contract.PathURL || r.CID != rawCID || r.Round != 42 || r.Src != contract.SrcBackfill || r.BlockTime.Unix() != 1_700_000_000 {
		t.Fatalf("ref: %+v", r)
	}
	if st, err := s.Reresolve(ctx, false); err != nil || st.Published != 0 || st.Scanned != 1 {
		t.Fatalf("second pass must publish nothing: %+v %v", st, err)
	}
}
