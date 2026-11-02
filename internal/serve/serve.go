// Package serve is seapin's read path: /ipfs/<cid>[/<path>] answered from the
// region's bucket. It never fetches from IPFS (D2): an object that is not
// cached is a 404.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"seapin/internal/catalog"
	"seapin/internal/cidkey"
	"seapin/internal/policy"
	"seapin/internal/store"
)

type Store interface {
	Stat(ctx context.Context, key string) (store.Info, error)
	GetRange(ctx context.Context, key string, off int64) (io.ReadCloser, error)
	Ping(ctx context.Context) error
}

type Catalog interface {
	Lookup(ctx context.Context, cid string) (catalog.Object, bool, error)
	ResolveAlias(ctx context.Context, root, subpath string) (string, bool, error)
	AssetObjects(ctx context.Context, chain string, asset uint64) ([]catalog.AssetObject, error)
	CIDAssets(ctx context.Context, chain, cid string, limit int) ([]catalog.CIDAsset, error)
	FetchState(ctx context.Context, root, subpath string) (catalog.FetchState, error)
	Demand(ctx context.Context, root, subpath, site string) error
	Ping(ctx context.Context) error
}

type Options struct {
	Store      Store
	Catalog    Catalog
	Takedowns  *Takedowns
	Log        *slog.Logger
	Registry   *prometheus.Registry
	MissMaxAge time.Duration
	// Read-through (D2): a request with DemandHeader: 1, which openresty sets
	// for whitelisted referers, queues a fetch on a miss and waits up to
	// DemandWait; at most DemandMaxWaiters requests wait at once.
	DemandHeader     string
	DemandWait       time.Duration
	DemandMaxWaiters int
	// PeerURL is the other region's internal listener: a local miss is
	// served from its cache when it has the object (PeerToken authorises).
	PeerURL, PeerToken string
	// ReadToken opens a read-only part of the internal listener (objects,
	// CID -> assets, the objects page, takedowns) to other services such as
	// nft-vectors; "" = off.
	ReadToken string
	// Sync serves this region's catalog to the peer's anti-entropy pull
	// (/internal/catalog/...); nil leaves those endpoints out.
	Sync SyncSource
}

// SyncSource is the catalog as the peer's anti-entropy reads it (*catalog.Catalog).
type SyncSource interface {
	RefsPage(ctx context.Context, since time.Time, after string, limit int) ([]catalog.SyncRef, error)
	ObjectsPage(ctx context.Context, since time.Time, after string, limit int, mimePrefix string) ([]catalog.SyncObject, error)
	AliasesPage(ctx context.Context, afterRoot, afterSub string, limit int) ([]catalog.SyncAlias, error)
	TakedownList(ctx context.Context) (map[string]string, error)
}

type Handler struct {
	Options
	requests *prometheus.CounterVec
	bytes    prometheus.Counter
	waiters  chan struct{}
	peer     *peer
}

func New(o Options) *Handler {
	h := &Handler{
		Options: o,
		peer:    newPeer(o.PeerURL, o.PeerToken),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seapin_serve_requests_total",
			Help: "Gateway requests by outcome.",
		}, []string{"outcome"}),
		bytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "seapin_serve_bytes_total",
			Help: "Response bytes written for /ipfs/ requests.",
		}),
		waiters: make(chan struct{}, max(1, o.DemandMaxWaiters)),
	}
	o.Registry.MustRegister(h.requests, h.bytes)
	return h
}

// Routes serves /ipfs/ for the front proxy plus /health and /metrics; the
// listener stays on loopback (D5).
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ipfs/{cid}", h.ipfs)
	mux.HandleFunc("GET /ipfs/{cid}/{path...}", h.ipfs)
	mux.HandleFunc("GET /health", h.health)
	mux.Handle("GET /metrics", promhttp.HandlerFor(h.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "seapin ipfs gateway")
	})
	return mux
}

const (
	outcomeHit           = "hit"
	outcomeMiss          = "miss"
	outcomePeer          = "peer" // served from the other region during replication lag
	outcomeBlocked       = "blocked"
	outcomeTakedown      = "takedown"
	outcomeInvalid       = "invalid"
	outcomeError         = "error"
	outcomeDemandHit     = "demand_hit"
	outcomeDemandTimeout = "demand_timeout"
	outcomeDemandBusy    = "demand_busy"
)

func (h *Handler) ipfs(w http.ResponseWriter, r *http.Request) {
	cw := &countingWriter{ResponseWriter: w}
	outcome := outcomeError
	defer func() {
		h.requests.WithLabelValues(outcome).Inc()
		h.bytes.Add(float64(cw.n))
	}()
	w = cw
	policy.SetHeaders(w.Header())

	root, err := cidkey.Normalize(r.PathValue("cid"))
	if err != nil {
		outcome = outcomeInvalid
		reply(w, http.StatusBadRequest, "public, max-age=86400", "invalid CID")
		return
	}
	sub, err := cidkey.CleanSubpath(r.PathValue("path"))
	if err != nil {
		outcome = outcomeInvalid
		reply(w, http.StatusBadRequest, "public, max-age=86400", "invalid path")
		return
	}
	if reason, ok := h.Takedowns.Reason(root); ok {
		outcome = outcomeTakedown
		h.takedown(w, root, reason)
		return
	}

	leaf := root
	if sub != "" {
		l, found, err := h.Catalog.ResolveAlias(r.Context(), root, sub)
		if err != nil {
			h.Log.Error("alias lookup", "root", root, "path", sub, "err", err)
			reply(w, http.StatusServiceUnavailable, "no-store", "catalog unavailable")
			return
		}
		if !found {
			outcome = h.notCached(w, r, root, sub)
			return
		}
		leaf = l
		if reason, ok := h.Takedowns.Reason(leaf); ok {
			outcome = outcomeTakedown
			h.takedown(w, leaf, reason)
			return
		}
	}
	outcome = h.serveLeaf(w, r, leaf, func() string {
		if h.blocked(w, r, leaf) {
			return outcomeBlocked
		}
		return h.notCached(w, r, root, sub)
	})
}

// serveLeaf streams a stored leaf object; missing answers when the bucket
// does not have it.
func (h *Handler) serveLeaf(w http.ResponseWriter, r *http.Request, leaf string, missing func() string) string {
	ctx := r.Context()
	key := cidkey.ObjectKey(leaf)
	info, err := h.Store.Stat(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return missing()
	}
	if err != nil {
		h.Log.Error("s3 stat", "cid", leaf, "err", err)
		reply(w, http.StatusBadGateway, "no-store", "storage error")
		return outcomeError
	}
	if !policy.Allowed(info.ContentType) {
		// Ingest never stores these; reaching here means the bucket was written
		// some other way.
		h.Log.Warn("stored object has a type that is not served", "cid", leaf, "type", info.ContentType)
		reply(w, http.StatusForbidden, "no-store", "content type not served")
		return outcomeBlocked
	}
	hdr := w.Header()
	hdr.Set("Content-Type", info.ContentType)
	hdr.Set("ETag", `"`+leaf+`"`)
	hdr.Set("Cache-Control", "public, max-age=29030400, immutable")
	obj := &objectReader{ctx: ctx, st: h.Store, key: key, size: info.Size}
	defer obj.Close()
	http.ServeContent(w, r, "", time.Time{}, obj)
	if obj.err != nil && ctx.Err() == nil {
		h.Log.Error("s3 read", "cid", leaf, "err", obj.err)
	}
	return outcomeHit
}

// blocked answers 403 when the catalog records the leaf as refused. Without
// the catalog it answers nothing: blocked bytes are never stored, so the
// caller's 404 stays safe.
func (h *Handler) blocked(w http.ResponseWriter, r *http.Request, leaf string) bool {
	o, found, err := h.Catalog.Lookup(r.Context(), leaf)
	if err != nil {
		h.Log.Warn("catalog lookup", "cid", leaf, "err", err)
		return false
	}
	if found && o.Status == catalog.StatusBlocked {
		reply(w, http.StatusForbidden, "public, max-age=86400", "content not served: "+o.Reason)
		return true
	}
	return false
}

// notCached answers for content the bucket does not have: read-through for
// whitelisted referers, otherwise a 404 the CDN may only keep briefly.
func (h *Handler) notCached(w http.ResponseWriter, r *http.Request, root, sub string) string {
	if h.fromPeer(w, r, root, sub) {
		return outcomePeer
	}
	if h.DemandHeader != "" && r.Header.Get(h.DemandHeader) == "1" {
		return h.demand(w, r, root, sub)
	}
	h.notFound(w)
	return outcomeMiss
}

func (h *Handler) notFound(w http.ResponseWriter) {
	reply(w, http.StatusNotFound, fmt.Sprintf("public, max-age=%d", int(h.MissMaxAge.Seconds())), "not cached")
}

func (h *Handler) takedown(w http.ResponseWriter, cid, reason string) {
	h.Log.Info("takedown served", "cid", cid, "reason", reason)
	reply(w, http.StatusUnavailableForLegalReasons, "public, max-age=3600", "unavailable for legal reasons")
}

// health is 503 only when the bucket is unreachable: cached objects are served
// from S3 alone, so a Postgres outage degrades path and miss answers but
// should not take the region out of rotation.
func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	res := map[string]string{"s3": "ok", "postgres": "ok"}
	code := http.StatusOK
	if err := h.Store.Ping(ctx); err != nil {
		res["s3"] = err.Error()
		code = http.StatusServiceUnavailable
	}
	if err := h.Catalog.Ping(ctx); err != nil {
		res["postgres"] = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(res)
}

func reply(w http.ResponseWriter, code int, cacheControl, msg string) {
	w.Header().Set("Cache-Control", cacheControl)
	http.Error(w, msg, code)
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
