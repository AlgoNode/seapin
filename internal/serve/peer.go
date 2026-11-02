package serve

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// peerHeader marks a request that came from the other region (every request
// on the internal listener carries it): such a request is never proxied on,
// so a miss cannot bounce between the regions.
const peerHeader = "X-Seapin-Peer"

const (
	peerSlots   = 64               // peer requests in flight at most; beyond that a miss is answered locally
	peerTTFB    = 2 * time.Second  // first byte from the peer, else answer locally
	peerNegTTL  = 60 * time.Second // a peer miss is not asked again for this long
	peerNegSize = 100_000          // the miss memory is dropped when it grows past this
)

// peer proxies local misses to the other region's cache during replication
// lag (PLAN §1, D2): only what the peer has cached and policy-checked itself.
type peer struct {
	url, token string
	client     *http.Client
	slots      chan struct{}

	mu  sync.Mutex
	neg map[string]time.Time
}

func newPeer(rawURL, token string) *peer {
	if rawURL == "" {
		return nil
	}
	return &peer{
		url: strings.TrimRight(rawURL, "/"), token: token, slots: make(chan struct{}, peerSlots),
		neg: map[string]time.Time{},
		client: &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: peerTTFB}).DialContext,
			ResponseHeaderTimeout: peerTTFB,
			MaxIdleConnsPerHost:   peerSlots,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
		}},
	}
}

func (p *peer) knownMiss(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.neg[key]
	if ok && time.Now().After(until) {
		delete(p.neg, key)
		return false
	}
	return ok
}

func (p *peer) rememberMiss(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.neg) >= peerNegSize {
		p.neg = map[string]time.Time{}
	}
	p.neg[key] = time.Now().Add(peerNegTTL)
}

// fromPeer serves root/sub from the peer region when it has it, and queues a
// local copy. It reports whether it answered the request.
func (h *Handler) fromPeer(w http.ResponseWriter, r *http.Request, root, sub string) bool {
	p := h.peer
	if p == nil || r.Header.Get(peerHeader) != "" {
		return false
	}
	key := root + "/" + sub
	if p.knownMiss(key) {
		return false
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return false
	}
	defer func() { <-p.slots }()

	u := p.url + "/internal/ipfs/" + root
	for seg := range strings.SplitSeq(sub, "/") {
		if seg != "" {
			u += "/" + url.PathEscape(seg)
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set(peerHeader, "1")
	for _, k := range []string{"Range", "If-None-Match"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified:
	case http.StatusNotFound, http.StatusPreconditionFailed:
		p.rememberMiss(key)
		return false
	default:
		return false
	}
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Cache-Control", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		io.Copy(w, resp.Body)
	}
	// queue a local copy in the read-through lane; ingest asks the peer first
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := h.Catalog.Demand(ctx, root, sub, "peer"); err != nil {
			h.Log.Warn("queue local copy of a peer hit", "root", root, "path", sub, "err", err)
		}
	}()
	return true
}
