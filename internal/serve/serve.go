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
	Ping(ctx context.Context) error
}

type Options struct {
	Store      Store
	Catalog    Catalog
	Takedowns  *Takedowns
	Log        *slog.Logger
	Registry   *prometheus.Registry
	MissMaxAge time.Duration
}

type Handler struct {
	Options
	requests *prometheus.CounterVec
	bytes    prometheus.Counter
}

func New(o Options) *Handler {
	h := &Handler{
		Options: o,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "seapin_serve_requests_total",
			Help: "Gateway requests by outcome.",
		}, []string{"outcome"}),
		bytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "seapin_serve_bytes_total",
			Help: "Response bytes written for /ipfs/ requests.",
		}),
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
	outcomeHit      = "hit"
	outcomeMiss     = "miss"
	outcomeBlocked  = "blocked"
	outcomeTakedown = "takedown"
	outcomeInvalid  = "invalid"
	outcomeError    = "error"
)

func (h *Handler) ipfs(w http.ResponseWriter, r *http.Request) {
	cw := &countingWriter{ResponseWriter: w}
	outcome := outcomeError
	defer func() {
		h.requests.WithLabelValues(outcome).Inc()
		h.bytes.Add(float64(cw.n))
	}()
	w = cw
	ctx := r.Context()
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
		l, found, err := h.Catalog.ResolveAlias(ctx, root, sub)
		if err != nil {
			h.Log.Error("alias lookup", "root", root, "path", sub, "err", err)
			reply(w, http.StatusServiceUnavailable, "no-store", "catalog unavailable")
			return
		}
		if !found {
			outcome = h.miss(w, r, root)
			return
		}
		leaf = l
		if reason, ok := h.Takedowns.Reason(leaf); ok {
			outcome = outcomeTakedown
			h.takedown(w, leaf, reason)
			return
		}
	}

	key := cidkey.ObjectKey(leaf)
	info, err := h.Store.Stat(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		outcome = h.miss(w, r, leaf)
		return
	}
	if err != nil {
		h.Log.Error("s3 stat", "cid", leaf, "err", err)
		reply(w, http.StatusBadGateway, "no-store", "storage error")
		return
	}
	if !policy.Allowed(info.ContentType) {
		// Ingest never stores these; reaching here means the bucket was written
		// some other way.
		h.Log.Warn("stored object has a type that is not served", "cid", leaf, "type", info.ContentType)
		outcome = outcomeBlocked
		reply(w, http.StatusForbidden, "no-store", "content type not served")
		return
	}

	outcome = outcomeHit
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
}

// miss answers for a CID that is not in the bucket: 403 when the catalog says
// it was refused, otherwise a 404 the CDN may only keep briefly.
func (h *Handler) miss(w http.ResponseWriter, r *http.Request, cid string) string {
	o, found, err := h.Catalog.Lookup(r.Context(), cid)
	if err != nil {
		// Blocked bytes are never stored, so a plain 404 is safe without the catalog.
		h.Log.Warn("catalog lookup", "cid", cid, "err", err)
	}
	if err == nil && found && o.Status == catalog.StatusBlocked {
		reply(w, http.StatusForbidden, "public, max-age=86400", "content not served: "+o.Reason)
		return outcomeBlocked
	}
	reply(w, http.StatusNotFound, fmt.Sprintf("public, max-age=%d", int(h.MissMaxAge.Seconds())), "not cached")
	return outcomeMiss
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
