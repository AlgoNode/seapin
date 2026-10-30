package serve

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"seapin/internal/catalog"
)

const whitelisted = "X-Seapin-Whitelisted"

func TestDemandFetchesForWhitelisted(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "unreferenced")
	f.cat.onDemand = func(root, sub string) { // ingest fetches it 300 ms later
		time.Sleep(300 * time.Millisecond)
		f.put(root, png, "image/png")
		f.cat.mu.Lock()
		f.cat.fetches[root+"/"+sub] = catalog.FetchState{Found: true, Status: "done", Leaf: root}
		f.cat.mu.Unlock()
	}
	rec := f.do("GET", "/ipfs/"+id, whitelisted, "1", "Referer", "https://nft.example/asset/1")
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Fatalf("got %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if len(f.cat.demanded) != 1 || f.cat.demanded[0] != id+"/@site:nft.example" {
		t.Errorf("demanded %v", f.cat.demanded)
	}
}

func TestDemandOnlyForWhitelisted(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "unreferenced")
	for _, v := range []string{"", "0", "yes"} {
		if rec := f.do("GET", "/ipfs/"+id, whitelisted, v); rec.Code != http.StatusNotFound {
			t.Errorf("header %q: got %d", v, rec.Code)
		}
	}
	if len(f.cat.demanded) != 0 {
		t.Errorf("non-whitelisted requests queued fetches: %v", f.cat.demanded)
	}
}

func TestDemandOutcomes(t *testing.T) {
	f := newFixture(t)
	id, _ := rawCID(t, "slow")
	start := time.Now()
	rec := f.do("GET", "/ipfs/"+id, whitelisted, "1")
	if rec.Code != http.StatusGatewayTimeout || rec.Header().Get("Retry-After") == "" || time.Since(start) < 2*time.Second {
		t.Errorf("still fetching after the wait: %d after %v", rec.Code, time.Since(start))
	}

	dead, _ := rawCID(t, "dead")
	f.cat.fetches[dead+"/"] = catalog.FetchState{Found: true, Status: "pending", Lane: catalog.LaneSlow}
	start = time.Now()
	if rec := f.do("GET", "/ipfs/"+dead, whitelisted, "1"); rec.Code != http.StatusNotFound || time.Since(start) > time.Second {
		t.Errorf("CID already retrying in the slow lane: %d after %v", rec.Code, time.Since(start))
	}
	if len(f.cat.demanded) != 1 {
		t.Errorf("a known-dead CID was queued again: %v", f.cat.demanded)
	}

	html, _ := rawCID(t, "html")
	f.cat.fetches[html+"/"] = catalog.FetchState{Found: true, Status: "done", Leaf: html}
	f.cat.objs[html] = catalog.Object{CID: html, Status: catalog.StatusBlocked, Reason: "not_allowed:text/html"}
	if rec := f.do("GET", "/ipfs/"+html, whitelisted, "1"); rec.Code != http.StatusForbidden {
		t.Errorf("fetched but refused by policy: %d", rec.Code)
	}
}

func TestDemandPathInsideDirectory(t *testing.T) {
	f := newFixture(t)
	root, _ := rawCID(t, "dir")
	leaf, _ := rawCID(t, "leaf")
	f.put(leaf, png, "image/png")
	f.cat.fetches[root+"/a/1.png"] = catalog.FetchState{Found: true, Status: "done", Leaf: leaf}
	rec := f.do("GET", "/ipfs/"+root+"/a/1.png", whitelisted, "1")
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"`+leaf+`"` {
		t.Errorf("got %d ETag %q", rec.Code, rec.Header().Get("ETag"))
	}
}
