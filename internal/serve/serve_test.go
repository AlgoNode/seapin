package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/prometheus/client_golang/prometheus"

	"seapin/internal/catalog"
	"seapin/internal/cidkey"
	"seapin/internal/store"
)

type fakeObject struct {
	data []byte
	ct   string
}

type fakeStore struct {
	mu     sync.Mutex
	objs   map[string]fakeObject
	ranges []int64 // offsets GetRange was called with
}

func (f *fakeStore) Stat(_ context.Context, key string) (store.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs[key]
	if !ok {
		return store.Info{}, store.ErrNotFound
	}
	return store.Info{Size: int64(len(o.data)), ContentType: o.ct}, nil
}

func (f *fakeStore) GetRange(_ context.Context, key string, off int64) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ranges = append(f.ranges, off)
	o, ok := f.objs[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.data[off:])), nil
}

func (f *fakeStore) Ping(context.Context) error { return nil }

type fakeCatalog struct {
	mu        sync.Mutex
	objs      map[string]catalog.Object
	aliases   map[string]string // root + "/" + subpath -> leaf
	assets    []catalog.AssetObject
	cidAssets []catalog.CIDAsset
	fetches   map[string]catalog.FetchState // root + "/" + subpath
	demanded  []string                      // root/subpath@site
	onDemand  func(root, sub string)        // plays ingest
	err       error
}

func (f *fakeCatalog) FetchState(_ context.Context, root, sub string) (catalog.FetchState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches[root+"/"+sub], f.err
}

func (f *fakeCatalog) Demand(_ context.Context, root, sub, site string) error {
	f.mu.Lock()
	f.demanded = append(f.demanded, root+"/"+sub+"@"+site)
	f.fetches[root+"/"+sub] = catalog.FetchState{Found: true, Status: "pending", Lane: catalog.LaneDemand}
	cb := f.onDemand
	f.mu.Unlock()
	if cb != nil {
		go cb(root, sub)
	}
	return f.err
}

func (f *fakeCatalog) Lookup(_ context.Context, cid string) (catalog.Object, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return catalog.Object{}, false, f.err
	}
	o, ok := f.objs[cid]
	return o, ok, nil
}

func (f *fakeCatalog) ResolveAlias(_ context.Context, root, sub string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", false, f.err
	}
	l, ok := f.aliases[root+"/"+sub]
	return l, ok, nil
}

func (f *fakeCatalog) Ping(context.Context) error { return f.err }

func (f *fakeCatalog) AssetObjects(context.Context, string, uint64) ([]catalog.AssetObject, error) {
	return f.assets, f.err
}

func (f *fakeCatalog) CIDAssets(_ context.Context, _, _ string, limit int) ([]catalog.CIDAsset, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cidAssets[:min(limit, len(f.cidAssets))], nil
}

func rawCID(t *testing.T, data string) (v1 string, v0 string) {
	t.Helper()
	sum, err := mh.Sum([]byte(data), mh.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	n, err := cidkey.Normalize(cid.NewCidV1(cid.Raw, sum).String())
	if err != nil {
		t.Fatal(err)
	}
	return n, cid.NewCidV0(sum).String()
}

type fixture struct {
	h   http.Handler
	st  *fakeStore
	cat *fakeCatalog
	td  *Takedowns
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		st:  &fakeStore{objs: map[string]fakeObject{}},
		cat: &fakeCatalog{objs: map[string]catalog.Object{}, aliases: map[string]string{}, fetches: map[string]catalog.FetchState{}},
		td:  &Takedowns{},
	}
	f.td.Set(map[string]string{})
	f.h = New(Options{
		Store: f.st, Catalog: f.cat, Takedowns: f.td,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry:   prometheus.NewRegistry(),
		MissMaxAge: time.Minute,

		DemandHeader:     "X-Seapin-Whitelisted",
		DemandWait:       2 * time.Second,
		DemandMaxWaiters: 4,
	}).Routes()
	return f
}

func (f *fixture) put(cid string, data []byte, ct string) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	f.st.objs[cidkey.ObjectKey(cid)] = fakeObject{data: data, ct: ct}
}

func (f *fixture) do(method, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

var png = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("pixels", 100))

func TestHit(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "a")
	f.put(id, png, "image/png")

	rec := f.do("GET", "/ipfs/"+id)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Fatalf("got %d, %d bytes", rec.Code, rec.Body.Len())
	}
	h := rec.Header()
	for k, want := range map[string]string{
		"Content-Type":           "image/png",
		"ETag":                   `"` + id + `"`,
		"Cache-Control":          "public, max-age=29030400, immutable",
		"X-Content-Type-Options": "nosniff",
		"Accept-Ranges":          "bytes",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("CSP = %q", h.Get("Content-Security-Policy"))
	}
}

func TestCIDv0RequestServesTheV1Object(t *testing.T) {
	f := newFixture(t)
	sum, _ := mh.Sum([]byte("dir"), mh.SHA2_256, -1)
	v0 := cid.NewCidV0(sum).String()
	v1, _ := cidkey.Normalize(v0)
	f.put(v1, png, "image/png")

	if rec := f.do("GET", "/ipfs/"+v0); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestRange(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "a")
	f.put(id, png, "image/png")

	rec := f.do("GET", "/ipfs/"+id, "Range", "bytes=100-199")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("got %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), png[100:200]) {
		t.Errorf("wrong bytes")
	}
	if got, want := rec.Header().Get("Content-Range"), "bytes 100-199/608"; got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
	if len(f.st.ranges) != 1 || f.st.ranges[0] != 100 {
		t.Errorf("S3 reads started at %v, want one read from 100", f.st.ranges)
	}

	if rec := f.do("GET", "/ipfs/"+id, "Range", "bytes=9000-"); rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("out-of-range: got %d", rec.Code)
	}
}

func TestHeadAndConditional(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "a")
	f.put(id, png, "image/png")

	rec := f.do("HEAD", "/ipfs/"+id)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "608" {
		t.Errorf("HEAD: %d, body %d, length %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
	rec = f.do("GET", "/ipfs/"+id, "If-None-Match", `"`+id+`"`)
	if rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: got %d", rec.Code)
	}
	if len(f.st.ranges) != 0 {
		t.Errorf("HEAD/304 read the object: %v", f.st.ranges)
	}
}

func TestMissIsShortLived404(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "missing")
	rec := f.do("GET", "/ipfs/"+id)
	if rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Errorf("got %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	// Without the catalog a miss is still a 404: blocked bytes are never stored.
	f.cat.err = errors.New("down")
	if rec := f.do("GET", "/ipfs/"+id); rec.Code != http.StatusNotFound {
		t.Errorf("catalog down: got %d", rec.Code)
	}
}

func TestBlockedIs403(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "html")
	f.cat.objs[id] = catalog.Object{CID: id, Status: catalog.StatusBlocked, Reason: "not_allowed:text/html"}
	rec := f.do("GET", "/ipfs/"+id)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not_allowed:text/html") {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestStoredDisallowedTypeIs403(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "planted")
	f.put(id, []byte("<html><script>alert(1)</script>"), "text/html")
	rec := f.do("GET", "/ipfs/"+id)
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "<script>") {
		t.Errorf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestTakedown(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "a")
	f.put(id, png, "image/png")
	f.td.Set(map[string]string{id: "dmca"})
	if rec := f.do("GET", "/ipfs/"+id); rec.Code != http.StatusUnavailableForLegalReasons {
		t.Errorf("got %d", rec.Code)
	}
}

func TestPathAlias(t *testing.T) {
	f := newFixture(t)
	root, _ := rawCID(t, "dir")
	leaf, _ := rawCID(t, "leaf")
	f.put(leaf, png, "image/png")
	f.cat.aliases[root+"/images/1.png"] = leaf

	if rec := f.do("GET", "/ipfs/"+root+"/images/1.png"); rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"`+leaf+`"` {
		t.Errorf("alias: got %d, ETag %q", rec.Code, rec.Header().Get("ETag"))
	}
	if rec := f.do("GET", "/ipfs/"+root+"/images/2.png"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: got %d", rec.Code)
	}
	f.td.Set(map[string]string{root: "dmca"})
	if rec := f.do("GET", "/ipfs/"+root+"/images/1.png"); rec.Code != http.StatusUnavailableForLegalReasons {
		t.Errorf("taken-down root: got %d", rec.Code)
	}
	f.td.Set(map[string]string{})
	f.cat.err = errors.New("down")
	if rec := f.do("GET", "/ipfs/"+root+"/images/1.png"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("catalog down: got %d", rec.Code)
	}
}

func TestInvalid(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "a")
	for _, p := range []string{"/ipfs/not-a-cid", "/ipfs/" + id + "/a%5Cb"} {
		rec := f.do("GET", p)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: got %d", p, rec.Code)
		}
	}
	if rec := f.do("POST", "/ipfs/"+id); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d", rec.Code)
	}
	if rec := f.do("GET", "/upload"); rec.Code != http.StatusNotFound {
		t.Errorf("/upload: got %d", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	f := newFixture(t)
	if rec := f.do("GET", "/health"); rec.Code != http.StatusOK {
		t.Errorf("healthy: got %d", rec.Code)
	}
	f.cat.err = errors.New("down")
	rec := f.do("GET", "/health")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "down") {
		t.Errorf("postgres down should stay 200 and say so: %d %s", rec.Code, rec.Body.String())
	}
}

func TestInternalCIDAssets(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.cat.cidAssets = append(f.cat.cidAssets, catalog.CIDAsset{Asset: uint64(10 + i), Key: fmt.Sprintf("mainnet-v1.0/%d/url", 10+i)})
	}
	h := New(Options{Store: f.st, Catalog: f.cat, Takedowns: f.td, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry: prometheus.NewRegistry()}).InternalRoutes(strings.Repeat("t", 32))
	_, v0 := rawCID(t, "a")
	call := func(path string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := call("/internal/cids/"+v0+"/assets", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := call("/internal/cids/notacid/assets", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cid: %d", rec.Code)
	}
	rec := call("/internal/cids/"+v0+"/assets?limit=2", true)
	var out struct {
		CID       string
		Assets    []catalog.CIDAsset
		Truncated bool
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, err)
	}
	if !strings.HasPrefix(out.CID, "baf") || len(out.Assets) != 2 || !out.Truncated || out.Assets[1].Asset != 11 {
		t.Fatalf("got %+v (CIDv0 must come back normalised, 2 of 3, truncated)", out)
	}
	if rec := call("/internal/cids/"+v0+"/assets", true); strings.Contains(rec.Body.String(), `"truncated":true`) {
		t.Fatalf("default limit truncated 3 assets: %s", rec.Body)
	}
}

// TestPeerProxy: a local miss is served from the peer region when the peer has
// the object (Range passed through) and a local copy is queued; a peer miss is
// answered locally and remembered; a peer's own request is never proxied on.
func TestPeerProxy(t *testing.T) {
	tok := strings.Repeat("p", 32)
	remote := newFixture(t)
	id, _ := rawCID(t, "on the peer only")
	remote.put(id, png, "image/png")
	remote.cat.objs[id] = catalog.Object{CID: id, Status: catalog.StatusCached, Mime: "image/png", Size: int64(len(png))}
	var peerHits atomic.Int32
	inner := New(Options{Store: remote.st, Catalog: remote.cat, Takedowns: remote.td,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Registry: prometheus.NewRegistry(), MissMaxAge: time.Minute}).InternalRoutes(tok)
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerHits.Add(1)
		inner.ServeHTTP(w, r)
	}))
	defer peerSrv.Close()

	local := newFixture(t)
	local.h = New(Options{Store: local.st, Catalog: local.cat, Takedowns: local.td,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Registry: prometheus.NewRegistry(), MissMaxAge: time.Minute,
		PeerURL: peerSrv.URL, PeerToken: tok}).Routes()

	rec := local.do("GET", "/ipfs/"+id)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), png) || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("peer hit: %d %q", rec.Code, rec.Header())
	}
	rec = local.do("GET", "/ipfs/"+id, "Range", "bytes=0-7")
	if rec.Code != 206 || rec.Body.Len() != 8 {
		t.Fatalf("range through the peer: %d len %d", rec.Code, rec.Body.Len())
	}
	waitFor(t, func() bool {
		local.cat.mu.Lock()
		defer local.cat.mu.Unlock()
		return len(local.cat.demanded) > 0 && local.cat.demanded[0] == id+"/@peer"
	})

	missing, _ := rawCID(t, "nowhere")
	before := peerHits.Load()
	for range 3 {
		if rec := local.do("GET", "/ipfs/"+missing); rec.Code != 404 {
			t.Fatalf("peer miss: %d", rec.Code)
		}
	}
	if n := peerHits.Load() - before; n != 1 {
		t.Fatalf("a peer miss must be remembered: asked the peer %d times", n)
	}

	before = peerHits.Load()
	other, _ := rawCID(t, "also nowhere")
	if rec := local.do("GET", "/ipfs/"+other, peerHeader, "1"); rec.Code != 404 || peerHits.Load() != before {
		t.Fatalf("a peer's request was proxied on: %d", rec.Code)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 100 {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

// TestInternalReadToken: the read token reaches objects, CID -> assets, the
// objects page and takedowns, nothing else; the full token reaches all.
func TestInternalReadToken(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "read token")
	f.put(id, png, "image/png")
	f.cat.objs[id] = catalog.Object{CID: id, Status: catalog.StatusCached, Mime: "image/png", Size: int64(len(png))}
	full, read := strings.Repeat("f", 32), strings.Repeat("r", 32)
	h := New(Options{Store: f.st, Catalog: f.cat, Sync: fakeSync{}, Takedowns: f.td, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry: prometheus.NewRegistry(), ReadToken: read}).InternalRoutes(full)
	call := func(path, tok string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for path, want := range map[string]int{
		"/internal/ipfs/" + id: 200, "/internal/cids/" + id + "/assets": 200,
		"/internal/catalog/objects?mime_prefix=image/": 200, "/internal/catalog/takedowns": 200,
		"/internal/catalog/refs": 401, "/internal/catalog/aliases": 401, "/internal/assets/mainnet-v1.0/1": 401,
	} {
		if got := call(path, read); got != want {
			t.Errorf("read token %s: %d, want %d", path, got, want)
		}
		if got := call(path, full); got == 401 {
			t.Errorf("full token %s: 401", path)
		}
	}
	if call("/internal/ipfs/"+id, "wrong-wrong-wrong-wrong-wrong-wrong") != 401 {
		t.Error("a wrong token got in")
	}
}

type fakeSync struct{}

func (fakeSync) RefsPage(context.Context, time.Time, string, int) ([]catalog.SyncRef, error) {
	return nil, nil
}
func (fakeSync) ObjectsPage(context.Context, time.Time, string, int, string) ([]catalog.SyncObject, error) {
	return nil, nil
}
func (fakeSync) AliasesPage(context.Context, string, string, int) ([]catalog.SyncAlias, error) {
	return nil, nil
}
func (fakeSync) TakedownList(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

// /internal/assets: JSON up to 10 KiB inline as content, a larger one as its first 10 KiB in head (the compat
// writer's ARC-3/NFD check).
func TestInternalAssetJSONHead(t *testing.T) {
	f := newFixture(t)
	small := []byte(`{"image":"ipfs://x"}`)
	big := []byte(`{"image":"ipfs://y","pad":"` + strings.Repeat("z", 12000) + `"}`)
	f.put("bafysmall", small, "application/json")
	f.put("bafybig", big, "application/json")
	f.cat.assets = []catalog.AssetObject{
		{Key: "mainnet-v1.0/1/url", Root: "bafysmall", Leaf: "bafysmall", Status: "cached", Mime: "application/json", Size: int64(len(small))},
		{Key: "mainnet-v1.0/1/url/x", Root: "bafybig", Leaf: "bafybig", Status: "cached", Mime: "application/json", Size: int64(len(big))},
	}
	h := New(Options{Store: f.st, Catalog: f.cat, Takedowns: f.td, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry: prometheus.NewRegistry()}).InternalRoutes(strings.Repeat("t", 32))
	req := httptest.NewRequest("GET", "/internal/assets/mainnet-v1.0/1", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 32))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		Objects []struct {
			Content json.RawMessage `json:"content"`
			Head    string          `json:"head"`
		} `json:"objects"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Objects) != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
	}
	if string(out.Objects[0].Content) != string(small) || out.Objects[0].Head != "" {
		t.Fatalf("small: %+v", out.Objects[0])
	}
	if out.Objects[1].Content != nil || len(out.Objects[1].Head) != 10<<10 || !strings.HasPrefix(out.Objects[1].Head, `{"image":"ipfs://y"`) {
		t.Fatalf("big: content %d head %d", len(out.Objects[1].Content), len(out.Objects[1].Head))
	}
}
