package fetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"seapin/internal/catalog"
	"seapin/internal/cidkey"
	"seapin/internal/store"
)

const (
	root = "bafybeie5gq4jxvzmsym6hjlwxej4rwdoxt7wadqvmmwbqi7r27fclha2va"
	leaf = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
)

var png = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("pixel"), 2000)...)

type memStore struct {
	objs map[string]memObj
}

type memObj struct {
	data []byte
	ct   string
}

func (m *memStore) Stat(_ context.Context, key string) (store.Info, error) {
	o, ok := m.objs[key]
	if !ok {
		return store.Info{}, store.ErrNotFound
	}
	return store.Info{Size: int64(len(o.data)), ContentType: o.ct}, nil
}

func (m *memStore) GetRange(_ context.Context, key string, off int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.objs[key].data[off:])), nil
}

func (m *memStore) Put(_ context.Context, key string, r io.Reader, size int64, ct string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(b)) != size {
		return io.ErrUnexpectedEOF
	}
	m.objs[key] = memObj{b, ct}
	return nil
}

type upstream struct {
	*httptest.Server
	hits atomic.Int64
	path atomic.Value
}

func serve(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.path.Store(r.URL.EscapedPath())
		h(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func file(body []byte, roots string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ipfs-Roots", roots)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}
}

func newFetcher(srcs ...Source) (*Fetcher, *memStore) {
	st := &memStore{objs: map[string]memObj{}}
	lim := DefaultLimits()
	lim.Stall = time.Second
	return &Fetcher{Sources: srcs, Client: NewClient(), Store: st, Limits: lim}, st
}

func TestFetchStoresLeaf(t *testing.T) {
	kubo := serve(t, file(png, root+","+leaf))
	f, st := newFetcher(Source{Name: "kubo", BaseURL: kubo.URL})
	res, err := f.Fetch(context.Background(), root, "images/My Image.png", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Leaf != leaf || res.Status != catalog.StatusCached || res.Mime != "image/png" || res.Size != int64(len(png)) || res.Source != "kubo" {
		t.Errorf("got %+v", res)
	}
	if o := st.objs[cidkey.ObjectKey(leaf)]; !bytes.Equal(o.data, png) || o.ct != "image/png" {
		t.Errorf("stored %d bytes as %q", len(o.data), o.ct)
	}
	if got := kubo.path.Load(); got != "/ipfs/"+root+"/images/My%20Image.png" {
		t.Errorf("requested %v", got)
	}
}

func TestFetchSkipsStoredRoot(t *testing.T) {
	kubo := serve(t, file(png, leaf))
	f, st := newFetcher(Source{Name: "kubo", BaseURL: kubo.URL})
	st.objs[cidkey.ObjectKey(leaf)] = memObj{png, "image/png"}
	res, err := f.Fetch(context.Background(), leaf, "", time.Second, false)
	if err != nil || res.Source != "local" || kubo.hits.Load() != 0 {
		t.Errorf("got %+v %v, %d upstream hits", res, err, kubo.hits.Load())
	}
}

func TestFetchBlocks(t *testing.T) {
	cases := map[string]struct {
		h      http.HandlerFunc
		reason string
	}{
		"html":    {file([]byte("<!DOCTYPE html><html><script>x</script>"), leaf), "not_allowed:text/html; charset=utf-8"},
		"too big": {file(bytes.Repeat([]byte("x"), 5000), leaf), "too_large"},
		"directory": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Etag", `"DirIndex-abc_CID-`+root+`"`)
			w.Header().Set("X-Ipfs-Roots", root)
			io.WriteString(w, "<html>index of</html>")
		}, "directory"},
		"too big, no length": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Ipfs-Roots", leaf)
			for range 10 {
				w.Write(png[:1000])
				w.(http.Flusher).Flush()
			}
		}, "too_large"},
	}
	for name, c := range cases {
		kubo := serve(t, c.h)
		f, st := newFetcher(Source{Name: "kubo", BaseURL: kubo.URL})
		f.Limits.MaxSize = 4000
		res, err := f.Fetch(context.Background(), root, "", time.Second, false)
		if err != nil || res.Status != catalog.StatusBlocked || res.Reason != c.reason {
			t.Errorf("%s: got %+v %v", name, res, err)
		}
		if len(st.objs) != 0 {
			t.Errorf("%s: something was stored", name)
		}
	}
}

func TestFetchJSONReturned(t *testing.T) {
	doc := []byte(`{"name":"x","image":"ipfs://` + leaf + `"}`)
	kubo := serve(t, file(doc, root))
	f, _ := newFetcher(Source{Name: "kubo", BaseURL: kubo.URL})
	res, err := f.Fetch(context.Background(), root, "", time.Second, false)
	if err != nil || res.Mime != "application/json" || !bytes.Equal(res.JSON, doc) {
		t.Errorf("got %+v %v", res, err)
	}
}

func TestFetchSourceOrder(t *testing.T) {
	peer := serve(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	kubo := serve(t, file(png, leaf))
	pinata := serve(t, file(png, leaf))
	f, _ := newFetcher(
		Source{Name: "peer", BaseURL: peer.URL, MissOK: true},
		Source{Name: "pinata", BaseURL: pinata.URL, SlowOnly: true},
		Source{Name: "kubo", BaseURL: kubo.URL},
	)
	res, err := f.Fetch(context.Background(), leaf, "", time.Second, false)
	if err != nil || res.Source != "kubo" || peer.hits.Load() != 1 || pinata.hits.Load() != 0 {
		t.Errorf("fast lane: %+v %v (peer %d, pinata %d)", res, err, peer.hits.Load(), pinata.hits.Load())
	}
}

func TestFetchFailures(t *testing.T) {
	slowHeaders := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	f, _ := newFetcher(Source{Name: "kubo", BaseURL: slowHeaders.URL})
	start := time.Now()
	_, err := f.Fetch(context.Background(), root, "", 200*time.Millisecond, false)
	if !errors.Is(err, errTTFB) || time.Since(start) > 2*time.Second {
		t.Errorf("TTFB: %v after %v", err, time.Since(start))
	}

	stalls := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ipfs-Roots", leaf)
		w.Header().Set("Content-Length", "100000")
		w.Write(png)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(10 * time.Second):
		case <-r.Context().Done():
		}
	})
	f, st := newFetcher(Source{Name: "kubo", BaseURL: stalls.URL})
	start = time.Now()
	_, err = f.Fetch(context.Background(), root, "", time.Second, false)
	if !errors.Is(err, errStall) || time.Since(start) > 5*time.Second || len(st.objs) != 0 {
		t.Errorf("stall: %v after %v, %d stored", err, time.Since(start), len(st.objs))
	}

	noLink := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `failed to resolve /ipfs/`+root+`/nope.png: no link named "nope.png" under `+root, http.StatusNotFound)
	})
	f, _ = newFetcher(Source{Name: "kubo", BaseURL: noLink.URL})
	if _, err := f.Fetch(context.Background(), root, "nope.png", time.Second, false); !errors.Is(err, ErrPermanent) {
		t.Errorf("a path missing from a resolved directory should be permanent: %v", err)
	}
	noBlock := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `failed to resolve /ipfs/`+root+`: block was not found locally (offline): ipld: could not find `+root, http.StatusNotFound)
	})
	f, _ = newFetcher(Source{Name: "kubo", BaseURL: noBlock.URL})
	if _, err := f.Fetch(context.Background(), root, "", time.Second, false); err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("a block kubo cannot get now should be retried: %v", err)
	}

	broken := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusBadGateway) })
	f, _ = newFetcher(Source{Name: "kubo", BaseURL: broken.URL})
	if _, err := f.Fetch(context.Background(), root, "", time.Second, false); err == nil || errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "502") {
		t.Errorf("502 should be retryable: %v", err)
	}

	noRoots := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write(png) })
	f, _ = newFetcher(Source{Name: "pinata", BaseURL: noRoots.URL})
	if _, err := f.Fetch(context.Background(), root, "a.png", time.Second, true); !errors.Is(err, errNoLeafCID) {
		t.Errorf("path without a leaf CID: %v", err)
	}
}

func TestFetchLegacyOnlyIfCached(t *testing.T) {
	var sawHeader atomic.Value
	legacyHas := atomic.Bool{}
	legacy := serve(t, func(w http.ResponseWriter, r *http.Request) {
		sawHeader.Store(r.Header.Get("Cache-Control"))
		if !legacyHas.Load() {
			http.Error(w, "not in cache", http.StatusPreconditionFailed)
			return
		}
		file(png, leaf)(w, r)
	})
	sidecar := serve(t, file(png, leaf))
	srcs := []Source{
		{Name: "legacy", BaseURL: legacy.URL, MissOK: true, Header: http.Header{"Cache-Control": {"only-if-cached"}}},
		{Name: "sidecar", BaseURL: sidecar.URL},
	}
	f, _ := newFetcher(srcs...)
	res, err := f.Fetch(context.Background(), leaf, "", time.Second, false)
	if err != nil || res.Source != "sidecar" || sawHeader.Load() != "only-if-cached" {
		t.Errorf("legacy miss: %+v %v, header %v", res, err, sawHeader.Load())
	}
	legacyHas.Store(true)
	f, _ = newFetcher(srcs...)
	if res, err := f.Fetch(context.Background(), leaf, "", time.Second, false); err != nil || res.Source != "legacy" || sidecar.hits.Load() != 1 {
		t.Errorf("legacy hit: %+v %v, sidecar hits %d", res, err, sidecar.hits.Load())
	}
}

func TestFetchDeadPeerBackoff(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close() // connections to it are refused now
	kubo := serve(t, file(png, leaf))
	f, _ := newFetcher(
		Source{Name: "peer", BaseURL: dead, MissOK: true},
		Source{Name: "sidecar-dead", BaseURL: dead + "/x"},
		Source{Name: "kubo", BaseURL: kubo.URL},
	)
	start := time.Now()
	res, err := f.Fetch(context.Background(), leaf, "", 10*time.Second, false)
	if err != nil || res.Source != "kubo" || time.Since(start) > 2*time.Second {
		t.Fatalf("dead peer: %+v %v after %v", res, err, time.Since(start))
	}
	if !f.skipped(f.Sources[0]) {
		t.Error("refused cache-only source not in backoff")
	}
	if f.skipped(f.Sources[1]) {
		t.Error("a source that is not cache-only must never be skipped")
	}
	f.down.Store("peer", time.Now().Add(-time.Second))
	if f.skipped(f.Sources[0]) {
		t.Error("backoff did not expire")
	}
}

// The filecoin source (a rainbow over a Filecoin provider's HTTP retrieval): slow lanes only, before the sidecar; a
// miss there is the remote backend's 502 and the next source is asked.
func TestFetchFilecoinSource(t *testing.T) {
	miss := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "failed to resolve /ipfs/x: Bad Gateway: http error from car gateway: 404 Not Found", http.StatusBadGateway)
	})
	hit := serve(t, file(png, leaf))
	sidecar := serve(t, file(png, leaf))
	f, _ := newFetcher(
		Source{Name: "filecoin", BaseURL: hit.URL, SlowOnly: true, MissOK: true},
		Source{Name: "sidecar", BaseURL: sidecar.URL},
	)
	if res, err := f.Fetch(context.Background(), leaf, "", time.Second, false); err != nil || res.Source != "sidecar" || hit.hits.Load() != 0 {
		t.Fatalf("fast lane must skip filecoin: %+v %v", res, err)
	}
	f, _ = newFetcher(f.Sources...) // empty store: the fast-lane fetch above stored the object
	if res, err := f.Fetch(context.Background(), leaf, "", time.Second, true); err != nil || res.Source != "filecoin" {
		t.Fatalf("slow lane: %+v %v", res, err)
	}
	f, _ = newFetcher(
		Source{Name: "filecoin", BaseURL: miss.URL, SlowOnly: true, MissOK: true},
		Source{Name: "sidecar", BaseURL: sidecar.URL},
	)
	if res, err := f.Fetch(context.Background(), leaf, "", time.Second, true); err != nil || res.Source != "sidecar" || miss.hits.Load() != 1 {
		t.Fatalf("filecoin miss must fall through: %+v %v", res, err)
	}
	if f.skipped(f.Sources[0]) {
		t.Fatal("a 502 miss must not put the source in connect backoff")
	}
}
