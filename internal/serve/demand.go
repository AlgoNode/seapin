package serve

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"seapin/internal/catalog"
)

// demand serves content nobody has cached yet to a whitelisted referer: it
// queues the fetch in ingest's demand lane and waits up to DemandWait. A CID
// whose first attempt already failed (it waits in the slow lane or was given
// up) answers 404 at once, so a dead CID costs one fetch, not one per request.
func (h *Handler) demand(w http.ResponseWriter, r *http.Request, root, sub string) string {
	select {
	case h.waiters <- struct{}{}:
		defer func() { <-h.waiters }()
	default:
		w.Header().Set("Retry-After", "30")
		reply(w, http.StatusServiceUnavailable, "no-store", "busy, retry later")
		return outcomeDemandBusy
	}
	ctx := r.Context()
	st, err := h.Catalog.FetchState(ctx, root, sub)
	if err == nil && (!st.Found || (st.Status == "pending" && st.Lane == catalog.LaneBackfill)) {
		err = h.Catalog.Demand(ctx, root, sub, site(r))
	}
	deadline := time.NewTimer(h.DemandWait)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if err != nil {
			h.Log.Error("read-through", "root", root, "path", sub, "err", err)
			reply(w, http.StatusServiceUnavailable, "no-store", "catalog unavailable")
			return outcomeError
		}
		switch {
		case st.Status == "done":
			return h.demandDone(w, r, st.Leaf)
		case st.Status == "failed", st.Status == "pending" && st.Lane == catalog.LaneSlow:
			h.notFound(w)
			return outcomeMiss
		}
		select {
		case <-ctx.Done():
			return outcomeError
		case <-deadline.C:
			w.Header().Set("Retry-After", "30")
			reply(w, http.StatusGatewayTimeout, "no-store", "still fetching, retry later")
			return outcomeDemandTimeout
		case <-tick.C:
		}
		st, err = h.Catalog.FetchState(ctx, root, sub)
	}
}

func (h *Handler) demandDone(w http.ResponseWriter, r *http.Request, leaf string) string {
	if reason, ok := h.Takedowns.Reason(leaf); ok {
		h.takedown(w, leaf, reason)
		return outcomeTakedown
	}
	out := h.serveLeaf(w, r, leaf, func() string {
		if h.blocked(w, r, leaf) {
			return outcomeBlocked
		}
		h.notFound(w)
		return outcomeMiss
	})
	if out == outcomeHit {
		return outcomeDemandHit
	}
	return out
}

// site is the read-through fair-share key: the referring site, as openresty
// names it in X-Seapin-Site, else the Referer's host.
func site(r *http.Request) string {
	s := r.Header.Get("X-Seapin-Site")
	if s == "" {
		if u, err := url.Parse(r.Referer()); err == nil {
			s = u.Hostname()
		}
	}
	if s == "" {
		s = "unknown"
	}
	return "site:" + strings.ToLower(truncate(s, 100))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
