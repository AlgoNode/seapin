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
	// a refused object is recorded under its detected type, without content, as the old scanner did
	// properties.file_url replaces image (the old scanner's last-wins per class)
	if m.Extra["json"].Type != "ARC-3" || m.Extra["img"].CID != "bafydir/1.png" || m.Reserve != r.String() ||
		m.Extra["oth"] == nil || m.Extra["oth"].MimeType != "text/html" || m.Extra["oth"].Content != nil {
		t.Errorf("meta %+v json %+v img %+v oth %+v", m, m.Extra["json"], m.Extra["img"], m.Extra["oth"])
	}
}

// CIDs keep the asset's spelling (mainnet asset 304870825: its URL says Qm…, seapin holds the CIDv1); a CID the
// asset wrote as v1, or a raw-leaf CID (no v0 form), stays as seapin has it.
func TestBuildMetaCIDSpelling(t *testing.T) {
	v1 := "bafybeibtwxnovbesjuk5437r7bwilfxzvmibxezj5b2ewy2xm7nxkca3fq"
	v0 := "QmRpWLRf2iHFRDSqW3JdBQPQjZ9PSw2i36jWxKttbnuPxo"
	m := buildMeta(dueAsset{id: 304870825, url: "ipfs://" + v0}, []resolvedObject{
		{Key: "c/304870825/url", Root: v1, Status: "cached", Mime: "image/png", Size: 1559297}})
	if m.Extra["img"].CID != v0 {
		t.Fatalf("v0 asset: %s", m.Extra["img"].CID)
	}
	m = buildMeta(dueAsset{id: 1, url: "ipfs://" + v1}, []resolvedObject{
		{Key: "c/1/url", Root: v1, Subpath: "a.png", Status: "cached", Mime: "image/png"}})
	if m.Extra["img"].CID != v1+"/a.png" {
		t.Fatalf("v1 asset: %s", m.Extra["img"].CID)
	}
	// a child referenced as v0 inside the JSON keeps v0
	m = buildMeta(dueAsset{id: 2, url: "ipfs://bafkreiaaa"}, []resolvedObject{
		{Key: "c/2/url", Root: "bafkreiaaa", Status: "cached", Mime: "application/json", Content: []byte(`{"image":"ipfs://` + v0 + `"}`)},
		{Key: "c/2/url/image", Root: v1, Status: "cached", Mime: "image/png"}})
	if m.Extra["img"].CID != v0 || m.Extra["json"].CID != "bafkreiaaa" {
		t.Fatalf("child: img %s json %s", m.Extra["img"].CID, m.Extra["json"].CID)
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

// Web links (not IPFS) are recorded as {url} only, never fetched, and never over an IPFS object of the same class.
func TestBuildMetaWebLinks(t *testing.T) {
	nfd := `{"name":"x.algo","image":"https://images.nf.domains/avatar/81f5d29b","properties":{"file_url":"https://e.com/a.mp4"}}`
	m := buildMeta(dueAsset{id: 765901320, url: "template-ipfs://{ipfscid:1:dag-pb:reserve:sha2-256}/nfd.json"}, []resolvedObject{
		{Key: "mainnet-v1.0/765901320/url", Root: "bafyjson", Status: "cached", Mime: "application/json", Content: []byte(nfd)}})
	if m.Extra["img"] == nil || m.Extra["img"].URL != "https://images.nf.domains/avatar/81f5d29b" || m.Extra["img"].MimeType != "" ||
		m.Extra["vid"] == nil || m.Extra["vid"].URL != "https://e.com/a.mp4" {
		t.Fatalf("nfd: img %+v vid %+v", m.Extra["img"], m.Extra["vid"])
	}
	// the asset URL itself is a web page
	m = buildMeta(dueAsset{id: 409755065, url: "https://ipfs.dahai.art/lot/59724"}, nil)
	if m.Extra["img"] == nil || m.Extra["img"].URL != "https://ipfs.dahai.art/lot/59724" {
		t.Fatalf("web asset url: %+v", m.Extra)
	}
	// an IPFS image wins; a gateway URL is not a web link (seapin resolves those)
	m = buildMeta(dueAsset{id: 3, url: "https://gateway.pinata.cloud/ipfs/bafypng"}, []resolvedObject{
		{Key: "mainnet-v1.0/3/url", Root: "bafypng", Status: "cached", Mime: "image/png"}})
	if m.Extra["img"].CID != "bafypng" || m.Extra["img"].URL != "" {
		t.Fatalf("ipfs wins: %+v", m.Extra["img"])
	}
}

// Real cases from the 2026-11-03 shadow comparison with svc-asset's rows.
func TestBuildMetaOldSpelling(t *testing.T) {
	// ARC-19 template asking for CIDv0 (asset 834758353)
	v1, v0 := "bafybeidsmpp3owdjyzahbqms73lhim6tuds634chms2kik47hb7nmuukti", "QmW3BWtGqqZFryNYS1AjmdMbAyC97qxvD1GPa2eAxTkofF"
	m := buildMeta(dueAsset{id: 834758353, url: "template-ipfs://{ipfscid:0:dag-pb:reserve:sha2-256}"}, []resolvedObject{
		{Key: "mainnet-v1.0/834758353/url", Root: v1, Status: "cached", Mime: "image/png", Size: 997406}})
	if m.Extra["img"].CID != v0 {
		t.Fatalf("template v0: %s", m.Extra["img"].CID)
	}
	// a path the URL wrote escaped (asset 492041505)
	qm := "QmeUnbHicJkXCTBvD6H6ZYYEzKzumiC15pN67ADpCtpVsT"
	m = buildMeta(dueAsset{id: 492041505, url: "ipfs://" + qm + "/ZombiePuddin%20%23321.png"}, []resolvedObject{
		{Key: "mainnet-v1.0/492041505/url", Root: v1of(t, qm), Subpath: "ZombiePuddin #321.png", Status: "cached", Mime: "image/png"}})
	if m.Extra["img"].CID != qm+"/ZombiePuddin%20%23321.png" {
		t.Fatalf("escaped path: %s", m.Extra["img"].CID)
	}
	// image and properties.file_url both images: file_url wins (asset 1804172142)
	m = buildMeta(dueAsset{id: 1804172142, url: "ipfs://bafkreijson#arc3"}, []resolvedObject{
		{Key: "mainnet-v1.0/1804172142/url", Root: "bafkreijson", Status: "cached", Mime: "application/json"},
		{Key: "mainnet-v1.0/1804172142/url/image", Root: "bafybeiimage", Status: "cached", Mime: "image/png"},
		{Key: "mainnet-v1.0/1804172142/url/properties/file_url", Root: "bafkreifile", Status: "cached", Mime: "image/png"}})
	if m.Extra["img"].CID != "bafkreifile" {
		t.Fatalf("last wins: %s", m.Extra["img"].CID)
	}
}

func v1of(t *testing.T, v0 string) string {
	t.Helper()
	c, err := cid.Decode(v0)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(c.Type(), c.Hash()).String()
}

// Links the old scanner did not follow only fill an empty class (cases from the 2026-11-03 comparison).
func TestBuildMetaOldLinksDecide(t *testing.T) {
	k := func(s string) string { return "mainnet-v1.0/880643891/" + s }
	big := `{"name":"x","image":"ipfs://bafybeiart","artifactUri":"ipfs://bafybeiart","thumbnailUri":"ipfs://bafybeithumb","pad":"` + strings.Repeat("z", 12000)
	m := buildMeta(dueAsset{id: 880643891, url: "ipfs://QmJSON"}, []resolvedObject{
		{Key: k("url"), Root: "bafyjson", Status: "cached", Mime: "application/json", Size: 11159, Head: big[:10240]},
		{Key: k("url/artifactUri"), Root: "bafybeiart", Status: "cached", Mime: "image/png", Size: 190015},
		{Key: k("url/assets/0/thumbnailUri"), Root: "bafybeithumb", Status: "cached", Mime: "image/jpeg", Size: 10788},
		{Key: k("url/image"), Root: "bafybeiart", Status: "cached", Mime: "image/png", Size: 190015},
		{Key: k("url/thumbnailUri"), Root: "bafybeithumb", Status: "cached", Mime: "image/jpeg", Size: 10788},
	})
	if m.Extra["img"].CID != "bafybeiart" || m.Extra["json"].Type != "ARC-3" {
		t.Fatalf("thumbnail won or no type: img %+v json %+v", m.Extra["img"], m.Extra["json"])
	}
	// ARC-69 note image: does not replace the URL's own image (1117782979), but fills an empty class
	k2 := func(s string) string { return "mainnet-v1.0/1117782979/" + s }
	v1, v0 := "bafybeihiroqxqosfzlumegz6moj6jyrgp42alkqyx5y7gg4ybxd453auqq", "QmdzQnWrYgyojPgKNp3mZ8oSkMVrNr7KRVeHbhh5htW18w"
	m = buildMeta(dueAsset{id: 1117782979, url: "ipfs://" + v0}, []resolvedObject{
		{Key: k2("note/image"), Root: v1, Status: "cached", Mime: "image/png"},
		{Key: k2("url"), Root: v1, Status: "cached", Mime: "image/png"},
	})
	if m.Extra["img"].CID != v0 {
		t.Fatalf("note replaced the url object: %+v", m.Extra["img"])
	}
	m = buildMeta(dueAsset{id: 5, url: "https://example.com/page"}, []resolvedObject{
		{Key: "mainnet-v1.0/5/note/animation_url", Root: "bafyvid", Status: "cached", Mime: "video/mp4"}})
	if m.Extra["vid"] == nil || m.Extra["vid"].CID != "bafyvid" {
		t.Fatalf("note should fill an empty class: %+v", m.Extra)
	}
}

func TestTopString(t *testing.T) {
	for text, want := range map[string]bool{
		`{"name":"a","image":"ipfs://x"}`:           true,
		`{"name":"a","image":"ipfs://x","pad":"zzz`: true, // cut off after the key: still found
		`{"name":"a","pad":"zzz`:                    false,
		`{"properties":{"image":"x"}}`:              false, // not top level
		`{"image":5}`:                               false,
		`not json`:                                  false,
	} {
		if topString(text, "image") != want {
			t.Errorf("%q: want %v", text, want)
		}
	}
}

// A v0 CID written inside a larger JSON (only its head is known) keeps v0 (asset 880643891).
func TestBuildMetaSpellingFromHead(t *testing.T) {
	v0 := "QmUyostGk4EgNXZLsqxSGR9kBMzChp5VxuUpphKLX5ZppA"
	v1 := v1of(t, v0)
	head := `{"name":"x","image":"ipfs://` + v0 + `","pad":"` + strings.Repeat("z", 10200)
	m := buildMeta(dueAsset{id: 880643891, url: "ipfs://bafybeijson"}, []resolvedObject{
		{Key: "mainnet-v1.0/880643891/url", Root: "bafybeijson", Status: "cached", Mime: "application/json", Head: head[:10240]},
		{Key: "mainnet-v1.0/880643891/url/image", Root: v1, Status: "cached", Mime: "image/png"}})
	if m.Extra["img"].CID != v0 {
		t.Fatalf("spelling from head: %s", m.Extra["img"].CID)
	}
}
