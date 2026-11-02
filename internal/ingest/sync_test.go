package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5"
	mh "github.com/multiformats/go-multihash"
	"github.com/prometheus/client_golang/prometheus"

	"seapin/internal/catalog"
	"seapin/internal/contract"
	"seapin/internal/serve"
)

func testCatalog(t *testing.T, name string) *catalog.Catalog {
	t.Helper()
	dsn := os.Getenv("SEAPIN_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SEAPIN_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("test_%s_%d", name, time.Now().UnixNano())
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn.Exec(ctx, "CREATE SCHEMA "+schema)
	t.Cleanup(func() {
		conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		conn.Close(context.Background())
	})
	cat, err := catalog.Open(ctx, dsn+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cat.Close)
	if err := cat.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return cat
}

func testCID(t *testing.T, s string) string {
	sum, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(cid.Raw, sum).String()
}

// TestSyncConverges: the local region pulls the peer's catalog and queues
// exactly what it lacks (a ref, a read-through object, a directory path, a
// takedown); a second run finds nothing to do.
func TestSyncConverges(t *testing.T) {
	ctx := context.Background()
	peer, local := testCatalog(t, "peer"), testCatalog(t, "local")
	c1, c2, c3, c4, c5, dir := testCID(t, "1"), testCID(t, "2"), testCID(t, "3"), testCID(t, "4"), testCID(t, "5"), testCID(t, "dir")
	ref := func(asset uint64, c string) contract.Ref {
		return contract.NewRef(contract.Ref{Chain: "mainnet-v1.0", Asset: asset, Path: contract.PathURL, CID: c,
			Creator: "C", Src: contract.SrcLive, Round: 7, BlockTime: time.Unix(1_700_000_000, 0)})
	}
	for _, r := range []contract.Ref{ref(1, c1), ref(2, c2)} {
		if _, err := peer.ApplyRef(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := local.ApplyRef(ctx, ref(1, c1)); err != nil { // the local region has the first one already
		t.Fatal(err)
	}
	if err := peer.MarkCached(ctx, c3, "image/png", 10, "sidecar"); err != nil { // read-through: no ref
		t.Fatal(err)
	}
	if _, err := peer.Complete(ctx, catalog.Job{Root: dir, Subpath: "a.png", Lane: catalog.LaneBackfill, Creator: "C"},
		catalog.Outcome{Leaf: c4, Status: catalog.StatusCached, Mime: "image/png", Size: 5, Source: "sidecar"}); err != nil {
		t.Fatal(err)
	}
	if err := peer.AddTakedown(ctx, c5, "dmca"); err != nil {
		t.Fatal(err)
	}

	tok := strings.Repeat("t", 32)
	srv := httptest.NewServer(serve.New(serve.Options{Catalog: peer, Sync: peer, Takedowns: &serve.Takedowns{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Registry: prometheus.NewRegistry()}).InternalRoutes(tok))
	defer srv.Close()
	s := &Syncer{PeerURL: srv.URL, Token: tok, Client: http.DefaultClient, Catalog: local, Overlap: time.Hour, Page: 1,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.Register(prometheus.NewRegistry())

	st, err := s.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Refs != 2 || st.RefsApplied != 1 || st.ObjectsQueued != 2 || st.AliasesQueued != 1 || st.Takedowns != 1 || st.Invalid != 0 {
		t.Fatalf("first run: %+v", st)
	}
	for _, f := range [][2]string{{c2, ""}, {c3, ""}, {c4, ""}, {dir, "a.png"}} {
		fs, err := local.FetchState(ctx, f[0], f[1])
		if err != nil || !fs.Found || fs.Status != "pending" || fs.Lane != catalog.LaneBackfill {
			t.Errorf("%s/%s not queued in backfill: %+v %v", f[0], f[1], fs, err)
		}
	}
	if td, _ := local.TakedownList(ctx); td[c5] != "dmca" {
		t.Errorf("takedown not copied: %v", td)
	}

	st, err = s.RunOnce(ctx)
	if err != nil || st.RefsApplied != 0 || st.ObjectsQueued != 0 || st.AliasesQueued != 0 || st.Takedowns != 0 {
		t.Fatalf("second run must change nothing: %+v %v", st, err)
	}
}
