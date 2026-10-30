package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"seapin/internal/catalog"
	"seapin/internal/contract"
	"seapin/internal/fetch"
)

// hangingFetcher makes every "flood" fetch hang until its deadline (an
// unfetchable CID) and answers the rest at once (a well-pinned one).
type hangingFetcher struct{}

func (hangingFetcher) Fetch(ctx context.Context, root, _ string, ttfb time.Duration, _ bool) (fetch.Result, error) {
	if strings.HasPrefix(root, "flood") {
		select {
		case <-time.After(ttfb):
		case <-ctx.Done():
		}
		return fetch.Result{}, fmt.Errorf("no first byte")
	}
	return fetch.Result{Leaf: root, Status: catalog.StatusCached, Mime: "image/png", Size: 1, Source: "kubo"}, nil
}

// TestFloodDoesNotBlockOthers runs the real claim queries: one creator queues
// 300 CIDs that each hang for the whole first-byte deadline, then another
// creator's CID arrives. It must be fetched at once, not after the flood.
func TestFloodDoesNotBlockOthers(t *testing.T) {
	dsn := os.Getenv("SEAPIN_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SEAPIN_TEST_PG_DSN not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	schema := fmt.Sprintf("test_flood_%d", time.Now().UnixNano())
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn.Exec(ctx, "CREATE SCHEMA "+schema)
	defer func() {
		conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		conn.Close(context.Background())
	}()
	cat, err := catalog.Open(ctx, dsn+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	ref := func(asset uint64, cid, creator string) contract.Ref {
		return contract.NewRef(contract.Ref{Chain: "mainnet-v1.0", Asset: asset, Path: "url", CID: cid,
			Creator: creator, Src: contract.SrcLive, Round: 1, BlockTime: time.Now()})
	}
	for i := range 300 {
		if _, err := cat.ApplyRef(ctx, ref(uint64(1000+i), fmt.Sprintf("flood-%03d", i), "FLOODER")); err != nil {
			t.Fatal(err)
		}
	}

	lanes := DefaultLanes()
	lanes[0].Slots, lanes[0].TTFB, lanes[0].RetryTTFB = 8, 3*time.Second, 3*time.Second
	s := &Scheduler{Catalog: cat, Fetcher: hangingFetcher{}, Events: &events{}, Lanes: lanes[:1],
		Region: "test", Ceiling: time.Minute, MaxLinks: 32, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.Register(prometheus.NewRegistry())
	go s.Run(ctx)
	time.Sleep(500 * time.Millisecond) // the flood now holds its slots

	start := time.Now()
	if _, err := cat.ApplyRef(ctx, ref(1, "good-nft", "ARTIST")); err != nil {
		t.Fatal(err)
	}
	s.Kick(catalog.LaneFast)
	var status string
	for time.Since(start) < 5*time.Second {
		conn.QueryRow(ctx, "SELECT status FROM "+schema+".fetches WHERE root_cid = 'good-nft'").Scan(&status)
		if status == "done" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	took := time.Since(start)
	var inflight int
	conn.QueryRow(ctx, "SELECT count(*) FROM "+schema+".fetches WHERE lease_until > now() AND root_cid LIKE 'flood%'").Scan(&inflight)
	if status != "done" || took > 1500*time.Millisecond {
		t.Fatalf("well-pinned CID: status %q after %v (flood TTFB 3 s)", status, took)
	}
	if inflight > lanes[0].CreatorCap {
		t.Errorf("flooding creator holds %d slots, cap is %d", inflight, lanes[0].CreatorCap)
	}
	t.Logf("well-pinned CID cached %v after it was queued, behind 300 hanging ones; flood held %d of %d slots", took.Round(time.Millisecond), inflight, lanes[0].Slots)
}
