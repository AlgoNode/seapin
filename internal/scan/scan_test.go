package scan

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/types"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mh "github.com/multiformats/go-multihash"
	"github.com/prometheus/client_golang/prometheus"

	"seapin/internal/contract"
)

func address(b byte) types.Address {
	var a types.Address
	for i := range a {
		a[i] = b
	}
	return a
}

func acfg(sender types.Address, asset uint64, p types.AssetParams, note string) types.SignedTxnWithAD {
	var s types.SignedTxnWithAD
	s.Txn.Type = types.AssetConfigTx
	s.Txn.Sender = sender
	s.Txn.ConfigAsset = types.AssetIndex(asset)
	s.Txn.AssetParams = p
	s.Txn.Note = []byte(note)
	return s
}

func TestEvents(t *testing.T) {
	create := acfg(address(1), 0, types.AssetParams{URL: "ipfs://x", Manager: address(1)}, "")
	create.ApplyData.ConfigAsset = 111
	inner := acfg(address(2), 0, types.AssetParams{URL: "ipfs://y"}, "")
	inner.ApplyData.ConfigAsset = 222
	app := types.SignedTxnWithAD{}
	app.Txn.Type = types.ApplicationCallTx
	app.ApplyData.EvalDelta.InnerTxns = []types.SignedTxnWithAD{inner, acfg(address(2), 111, types.AssetParams{Reserve: address(3)}, "")}
	pay := types.SignedTxnWithAD{}
	pay.Txn.Type = types.PaymentTx
	destroy := acfg(address(1), 333, types.AssetParams{}, "")

	b := types.Block{BlockHeader: types.BlockHeader{Round: 42, TimeStamp: 1_700_000_000, GenesisID: "mainnet-v1.0"}}
	for _, s := range []types.SignedTxnWithAD{create, pay, app, destroy} {
		b.Payset = append(b.Payset, types.SignedTxnInBlock{SignedTxnWithAD: s, HasGenesisID: true})
	}
	evs := Events(b)
	var got []string
	for _, e := range evs {
		got = append(got, fmt.Sprintf("%d:%d", e.Asset, e.Kind))
	}
	if strings.Join(got, " ") != "111:1 222:1 111:2 333:3" {
		t.Fatalf("events %v", got)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq <= evs[i-1].Seq || evs[i].Seq>>20 != 42 {
			t.Errorf("sequence not increasing within round 42: %v", evs)
		}
	}
	if evs[0].TxID == "" || evs[1].TxID != "" || evs[0].Time.Unix() != 1_700_000_000 {
		t.Errorf("txid only for top-level, block time kept: %+v", evs[:2])
	}
}

// TestEventsOldCreateID: before round ~16.69 M blocks do not record a created
// asset's id; it follows from the block's transaction counter. Mainnet round
// 14,936,396: TxnCounter 271,852,356, 72 transactions, the creation at index
// 20 is asset 271,852,305.
func TestEventsOldCreateID(t *testing.T) {
	b := types.Block{BlockHeader: types.BlockHeader{Round: 14936396, GenesisID: "mainnet-v1.0"}}
	b.TxnCounter = 271852356
	for i := range 72 {
		s := types.SignedTxnWithAD{}
		s.Txn.Type = types.PaymentTx
		if i == 20 {
			s = acfg(address(1), 0, types.AssetParams{URL: "ipfs://x"}, "")
		}
		b.Payset = append(b.Payset, types.SignedTxnInBlock{SignedTxnWithAD: s})
	}
	evs := Events(b)
	if len(evs) != 1 || evs[0].Asset != 271852305 || evs[0].Kind != Create {
		t.Fatalf("old creation: %+v", evs)
	}
	// With inner transactions in the block the counter arithmetic does not hold:
	// a creation without a recorded id is skipped, as before.
	app := types.SignedTxnWithAD{}
	app.Txn.Type = types.ApplicationCallTx
	app.ApplyData.EvalDelta.InnerTxns = []types.SignedTxnWithAD{{}}
	b.Payset[0] = types.SignedTxnInBlock{SignedTxnWithAD: app}
	if evs := Events(b); len(evs) != 0 {
		t.Fatalf("derived an id in a block with inner transactions: %+v", evs)
	}
}

// openScanner returns a scanner on a throwaway schema of SEAPIN_TEST_PG_DSN.
func openScanner(t *testing.T) *Scanner {
	t.Helper()
	dsn := os.Getenv("SEAPIN_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SEAPIN_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("test_scan_%d", time.Now().UnixNano())
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn.Exec(ctx, "CREATE SCHEMA "+schema)
	t.Cleanup(func() {
		conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		conn.Close(context.Background())
	})
	pool, err := pgxpool.New(ctx, dsn+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := &Scanner{Chain: "mainnet-v1.0", DB: pool, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *Scanner) applyAll(t *testing.T, src string, evs ...Event) []contract.Ref {
	t.Helper()
	ctx := context.Background()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	refs, err := s.applyEvents(ctx, tx, evs, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return refs
}

func ev(asset uint64, kind Kind, round uint64, p types.AssetParams, note string) Event {
	return Event{Asset: asset, Kind: kind, Round: round, Seq: int64(round) << 20, Time: time.Unix(1_700_000_000, 0),
		Sender: address(9), Params: p, Note: []byte(note)}
}

// arc19 is the CID an ARC-19 raw template resolves to for address a.
func arc19(a types.Address) string {
	sum, _ := mh.Encode(a[:], mh.SHA2_256)
	return cid.NewCidV1(cid.Raw, sum).String()
}

const template = "template-ipfs://{ipfscid:1:raw:reserve:sha2-256}"

func TestApplyBackfillNewestFirst(t *testing.T) {
	s := openScanner(t)
	// Walking down: the newest reconfiguration first (no URL yet), an older
	// one (skipped), then the creation, which supplies the URL.
	refs := s.applyAll(t, contract.SrcBackfill,
		ev(5, Reconfig, 300, types.AssetParams{Reserve: address(2), Manager: address(9)}, ""),
		ev(5, Reconfig, 200, types.AssetParams{Reserve: address(1), Manager: address(9)}, ""),
		ev(5, Create, 100, types.AssetParams{URL: template, Reserve: address(0xa), AssetName: "Dyn", UnitName: "D"}, ""),
	)
	if len(refs) != 1 || refs[0].CID != arc19(address(2)) || refs[0].Round != 300 || refs[0].Src != contract.SrcBackfill || refs[0].Key != "mainnet-v1.0/5/url" {
		t.Fatalf("refs %+v", refs)
	}
	var refRound int64
	s.DB.QueryRow(context.Background(), `SELECT ref_round FROM scan_assets WHERE asset = 5`).Scan(&refRound)
	if refRound != 300 {
		t.Errorf("ref_round %d", refRound)
	}

	// A destroyed asset (destroy seen first) produces nothing.
	if refs := s.applyAll(t, contract.SrcBackfill,
		ev(8, Destroy, 500, types.AssetParams{}, ""),
		ev(8, Create, 100, types.AssetParams{URL: "ipfs://bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"}, ""),
	); len(refs) != 0 {
		t.Errorf("destroyed asset: %+v", refs)
	}
}

func TestApplyLive(t *testing.T) {
	s := openScanner(t)
	note := `{"standard":"arc69","media_url":"ipfs://bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"}`
	refs := s.applyAll(t, contract.SrcLive,
		ev(6, Create, 1000, types.AssetParams{URL: "ipfs://QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG#arc3"}, note))
	var paths []string
	for _, r := range refs {
		paths = append(paths, r.Path)
	}
	if strings.Join(paths, " ") != "note/media_url url" {
		t.Fatalf("live create: %v", paths)
	}
	if refs[1].Creator != address(9).String() {
		t.Errorf("creator %q", refs[1].Creator)
	}

	// A reconfiguration of an asset the backfill has not reached: the URL
	// comes from algod, the CID from the new reserve.
	looked := 0
	s.Lookup = func(_ context.Context, id uint64) (string, string, string, string, bool, error) {
		looked++
		return template, "Dyn", "D", "CREATOR", true, nil
	}
	refs = s.applyAll(t, contract.SrcLive, ev(7, Reconfig, 2000, types.AssetParams{Reserve: address(4)}, ""))
	if looked != 1 || len(refs) != 1 || refs[0].CID != arc19(address(4)) || refs[0].Creator != "CREATOR" {
		t.Fatalf("live reconfig: looked %d, refs %+v", looked, refs)
	}
	// The next reconfiguration needs no lookup and moves the ref to the new CID.
	refs = s.applyAll(t, contract.SrcLive, ev(7, Reconfig, 2001, types.AssetParams{Reserve: address(5)}, ""))
	if looked != 1 || len(refs) != 1 || refs[0].CID != arc19(address(5)) || refs[0].Round != 2001 {
		t.Fatalf("second reconfig: looked %d, refs %+v", looked, refs)
	}
	// Replaying an older configuration (a restart) changes nothing.
	if refs := s.applyAll(t, contract.SrcLive, ev(7, Reconfig, 2000, types.AssetParams{Reserve: address(4)}, "")); len(refs) != 0 {
		t.Errorf("replayed configuration produced %+v", refs)
	}
}

func TestBuildMeta(t *testing.T) {
	objs := []resolvedObject{
		{Key: "c/1/url", Root: "bafyjson", Status: "cached", Mime: "application/json", Size: 50, Content: []byte(`{"image":"ipfs://x"}`)},
		{Key: "c/1/url/image", Root: "bafypng", Status: "cached", Mime: "image/png", Size: 70},
		{Key: "c/1/url/properties/file_url", Root: "bafydir", Subpath: "1.png", Status: "cached", Mime: "image/png", Size: 80},
		{Key: "c/1/url/animation_url", Root: "bafyhtml", Status: "blocked", Mime: "text/html"},
	}
	r := address(3)
	m := buildMeta(dueAsset{id: 1, name: "N", url: "ipfs://bafyjson", reserve: r[:], refRound: 9}, objs)
	if m.Extra["json"].Type != "ARC-3" || m.Extra["img"].CID != "bafypng" || m.Extra["oth"] != nil || m.Reserve != r.String() {
		t.Errorf("meta %+v json %+v img %+v", m, m.Extra["json"], m.Extra["img"])
	}
}

func newRegistry() *prometheus.Registry { return prometheus.NewRegistry() }

// Asset parameters are arbitrary bytes on chain (mainnet asset 3753495419 has
// a NUL); they must not fail the block's transaction.
func TestApplyBinaryText(t *testing.T) {
	s := openScanner(t)
	refs := s.applyAll(t, contract.SrcBackfill, ev(11, Create, 100, types.AssetParams{
		URL:       "ipfs://bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy\x00\x00",
		AssetName: "A\x00B\xff", UnitName: "\x00",
	}, ""))
	if len(refs) != 1 || refs[0].CID != "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy" {
		t.Fatalf("refs %+v", refs)
	}
	var name string
	var unit *string
	s.DB.QueryRow(context.Background(), `SELECT name, unit FROM scan_assets WHERE asset = 11`).Scan(&name, &unit)
	if name != "AB\uFFFD" || unit != nil {
		t.Errorf("name %q unit %v", name, unit)
	}
}

func TestMetaTable(t *testing.T) {
	for in, want := range map[string]string{"": "x_asset_meta", "x_asset_meta": `"x_asset_meta"`,
		"seapin_shadow.x_asset_meta": `"seapin_shadow"."x_asset_meta"`} {
		if got := (&MetaWriter{Table: in}).table(); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}
