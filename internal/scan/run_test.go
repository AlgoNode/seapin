package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		create := acfg(address(byte(round)), 0, types.AssetParams{URL: "ipfs://" + rawCID + "/" + strconv.FormatUint(round, 10) + ".json"}, "")
		create.ApplyData.ConfigAsset = types.AssetIndex(round)
		b := types.Block{BlockHeader: types.BlockHeader{Round: types.Round(round), TimeStamp: 1_700_000_000, GenesisID: "mainnet-v1.0"}}
		b.Payset = []types.SignedTxnInBlock{{SignedTxnWithAD: create, HasGenesisID: true}}
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
