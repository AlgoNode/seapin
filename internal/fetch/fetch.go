// Package fetch gets one object for ingest from an ordered list of sources
// (the other region's seapin, the old gateway's caches during the migration,
// the IPFS gateway sidecar, Pinata) and stores it when the
// content policy allows. Deadlines measure progress rather than total time:
// time to first byte, then stalls, a minimum rate and a hard ceiling (PLAN §1
// Ingest scheduling).
package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"seapin/internal/catalog"
	"seapin/internal/cidkey"
	"seapin/internal/policy"
	"seapin/internal/store"
)

// Source is one place to fetch from. "<BaseURL>/ipfs/<cid>[/<path>]" is requested.
type Source struct {
	Name     string
	BaseURL  string
	Header   http.Header // e.g. the peer's bearer token, Pinata's gateway token
	SlowOnly bool        // only tried outside the fast lane (Pinata)
	MissOK   bool        // 404 or 412 means "not here, ask the next source" (cache-only sources: the peer, legacy)
}

type Store interface {
	Stat(ctx context.Context, key string) (store.Info, error)
	GetRange(ctx context.Context, key string, off int64) (io.ReadCloser, error)
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
}

type Limits struct {
	MaxSize   int64         // larger objects are refused (too_large)
	Stall     time.Duration // abort when no byte arrives for this long
	MinRate   int64         // bytes per second, enforced once RateGrace has passed
	RateGrace time.Duration
	Ceiling   time.Duration // hard limit for one transfer
	MaxJSON   int64         // JSON objects up to this size are returned for link extraction
}

func DefaultLimits() Limits {
	return Limits{
		MaxSize:   policy.MaxObjectSize,
		Stall:     15 * time.Second,
		MinRate:   32 << 10,
		RateGrace: 30 * time.Second,
		Ceiling:   10 * time.Minute,
		MaxJSON:   1 << 20,
	}
}

type Fetcher struct {
	Sources []Source
	Client  *http.Client
	Store   Store
	Limits  Limits
}

// Result is the leaf object a fetch produced, stored (cached) or refused (blocked).
type Result struct {
	Leaf   string
	Status catalog.Status
	Mime   string // served type when cached, detected type when blocked
	Size   int64
	Reason string
	Source string
	JSON   []byte // the object itself when it is JSON of at most MaxJSON bytes
}

var (
	// ErrPermanent marks failures retrying will not fix (the path does not exist).
	ErrPermanent = errors.New("permanent")

	errNotHere    = errors.New("not at this source")
	errTTFB       = errors.New("no first byte before the lane deadline")
	errStall      = errors.New("transfer stalled")
	errSlow       = errors.New("transfer below the minimum rate")
	errCeiling    = errors.New("transfer over the time ceiling")
	errTooLarge   = errors.New("object over the size cap")
	errNoLeafCID  = errors.New("source did not say which CID the path resolves to")
	errEmptyChain = errors.New("no source has the object")
)

// NewClient is the HTTP client for sources: no overall timeout (Fetch sets the
// deadlines), no transparent decompression, redirects only within one host.
func NewClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			MaxIdleConnsPerHost: 64,
			DisableCompression:  true,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.Host != via[0].URL.Host {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// Fetch resolves subpath inside root and stores the leaf object. Sources are
// tried in order; slow allows the SlowOnly ones. ttfb is the lane's deadline
// for the first byte.
func (f *Fetcher) Fetch(ctx context.Context, root, subpath string, ttfb time.Duration, slow bool) (Result, error) {
	if subpath == "" {
		if res, ok := f.stored(ctx, root, "local"); ok {
			return res, nil
		}
	}
	var errs []error
	for _, src := range f.Sources {
		if src.SlowOnly && !slow {
			continue
		}
		res, err := f.fromSource(ctx, src, root, subpath, ttfb)
		switch {
		case err == nil:
			return res, nil
		case errors.Is(err, errNotHere):
		case errors.Is(err, ErrPermanent):
			return Result{}, fmt.Errorf("%s: %w", src.Name, err)
		default:
			errs = append(errs, fmt.Errorf("%s: %w", src.Name, err))
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
	}
	if len(errs) == 0 {
		return Result{}, errEmptyChain
	}
	return Result{}, errors.Join(errs...)
}

// stored returns the result for a leaf already in the bucket, reading it back
// when it is JSON so its links can be (re)recorded.
func (f *Fetcher) stored(ctx context.Context, leaf, source string) (Result, bool) {
	info, err := f.Store.Stat(ctx, cidkey.ObjectKey(leaf))
	if err != nil {
		return Result{}, false
	}
	res := Result{Leaf: leaf, Status: catalog.StatusCached, Mime: info.ContentType, Size: info.Size, Source: source}
	if info.ContentType == "application/json" && info.Size <= f.Limits.MaxJSON {
		if body, err := f.Store.GetRange(ctx, cidkey.ObjectKey(leaf), 0); err == nil {
			res.JSON, _ = io.ReadAll(io.LimitReader(body, f.Limits.MaxJSON))
			body.Close()
		}
	}
	return res, true
}

func (f *Fetcher) fromSource(parent context.Context, src Source, root, subpath string, ttfb time.Duration) (Result, error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	ttfbTimer := time.AfterFunc(ttfb, func() { cancel(errTTFB) })
	defer ttfbTimer.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, objectURL(src.BaseURL, root, subpath), nil)
	if err != nil {
		return Result{}, err
	}
	maps.Copy(req.Header, src.Header)
	resp, err := f.Client.Do(req)
	if err != nil {
		return Result{}, causeOf(ctx, err)
	}
	defer resp.Body.Close()

	switch code := resp.StatusCode; {
	case code == http.StatusOK:
	case (code == http.StatusNotFound || code == http.StatusPreconditionFailed) && src.MissOK:
		return Result{}, errNotHere
	case code == http.StatusBadRequest || code == http.StatusGone:
		return Result{}, fmt.Errorf("%w: http %d", ErrPermanent, code)
	case code == http.StatusNotFound:
		// The sidecar (rainbow; kubo answers the same, both are boxo gateways)
		// answers 404 both for a path that does not exist in a resolved
		// directory (final) and for a block it cannot get (may appear later).
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if bytes.Contains(msg, []byte("no link named")) {
			return Result{}, fmt.Errorf("%w: %s", ErrPermanent, bytes.TrimSpace(msg))
		}
		return Result{}, fmt.Errorf("http 404: %s", bytes.TrimSpace(msg))
	default:
		return Result{}, fmt.Errorf("http %d", code)
	}

	leaf, err := leafCID(resp.Header, root, subpath)
	if err != nil {
		return Result{}, err
	}
	blocked := Result{Leaf: leaf, Status: catalog.StatusBlocked, Source: src.Name}
	if isDirListing(resp.Header) {
		blocked.Mime, blocked.Reason = "text/html", "directory"
		return blocked, nil
	}
	if resp.ContentLength > f.Limits.MaxSize {
		blocked.Mime, blocked.Size, blocked.Reason = resp.Header.Get("Content-Type"), resp.ContentLength, "too_large"
		return blocked, nil
	}
	if res, ok := f.stored(ctx, leaf, src.Name); ok {
		return res, nil
	}

	w := newWatched(resp.Body, f.Limits, cancel, func() { ttfbTimer.Stop() })
	defer w.stop()
	head := make([]byte, policy.SniffLen)
	n, err := io.ReadFull(w, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Result{}, causeOf(ctx, err)
	}
	head = head[:n]
	contentType, detected, ok := policy.Classify(head)
	if !ok {
		blocked.Mime, blocked.Size, blocked.Reason = detected, resp.ContentLength, "not_allowed:"+detected
		return blocked, nil
	}

	body := io.MultiReader(bytes.NewReader(head), w)
	var jsonBuf *capBuffer
	if contentType == "application/json" && resp.ContentLength <= f.Limits.MaxJSON {
		jsonBuf = &capBuffer{max: f.Limits.MaxJSON}
		body = io.TeeReader(body, jsonBuf)
	}
	capped := &capReader{r: body, max: f.Limits.MaxSize}
	if err := f.Store.Put(ctx, cidkey.ObjectKey(leaf), capped, resp.ContentLength, contentType); err != nil {
		if capped.over {
			blocked.Mime, blocked.Size, blocked.Reason = detected, capped.n, "too_large"
			return blocked, nil
		}
		return Result{}, fmt.Errorf("store: %w", causeOf(ctx, err))
	}
	res := Result{Leaf: leaf, Status: catalog.StatusCached, Mime: contentType, Size: capped.n, Source: src.Name}
	if jsonBuf != nil && !jsonBuf.over {
		res.JSON = jsonBuf.Bytes()
	}
	return res, nil
}

func objectURL(base, root, subpath string) string {
	var u strings.Builder
	u.WriteString(strings.TrimRight(base, "/") + "/ipfs/" + root)
	for seg := range strings.SplitSeq(subpath, "/") {
		if seg != "" {
			u.WriteString("/" + url.PathEscape(seg))
		}
	}
	return u.String()
}

// leafCID is the CID the request resolved to: the last X-Ipfs-Roots entry
// (boxo gateways: rainbow, kubo), else a CID ETag (boxo, seapin), else the
// root when there is no path.
func leafCID(h http.Header, root, subpath string) (string, error) {
	if roots := h.Get("X-Ipfs-Roots"); roots != "" {
		parts := strings.Split(roots, ",")
		return cidkey.Normalize(strings.TrimSpace(parts[len(parts)-1]))
	}
	etag := strings.Trim(strings.TrimPrefix(h.Get("Etag"), "W/"), `"`)
	if c, err := cidkey.Normalize(etag); err == nil {
		return c, nil
	}
	if subpath == "" {
		return root, nil
	}
	return "", errNoLeafCID
}

func isDirListing(h http.Header) bool {
	return strings.Contains(h.Get("Etag"), "DirIndex-")
}

func causeOf(ctx context.Context, err error) error {
	if c := context.Cause(ctx); c != nil && !errors.Is(c, context.Canceled) {
		return c
	}
	return err
}

// watched is a body reader with a watchdog: after the first byte it cancels
// the transfer on a stall, a rate below the minimum or the time ceiling.
type watched struct {
	r       io.Reader
	lim     Limits
	cancel  context.CancelCauseFunc
	onFirst func()
	first   sync.Once
	startNs atomic.Int64
	lastNs  atomic.Int64
	n       atomic.Int64
	done    chan struct{}
	stopper sync.Once
}

func newWatched(r io.Reader, lim Limits, cancel context.CancelCauseFunc, onFirst func()) *watched {
	w := &watched{r: r, lim: lim, cancel: cancel, onFirst: onFirst, done: make(chan struct{})}
	go w.run()
	return w
}

func (w *watched) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		now := time.Now().UnixNano()
		w.first.Do(func() {
			w.startNs.Store(now)
			w.onFirst()
		})
		w.lastNs.Store(now)
		w.n.Add(int64(n))
	}
	return n, err
}

func (w *watched) run() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-w.done:
			return
		case now := <-tick.C:
			start := w.startNs.Load()
			if start == 0 {
				continue // still waiting for the first byte: the TTFB timer decides
			}
			elapsed := now.Sub(time.Unix(0, start))
			switch {
			case now.Sub(time.Unix(0, w.lastNs.Load())) > w.lim.Stall:
				w.cancel(errStall)
			case elapsed > w.lim.Ceiling:
				w.cancel(errCeiling)
			case elapsed > w.lim.RateGrace && float64(w.n.Load())/elapsed.Seconds() < float64(w.lim.MinRate):
				w.cancel(errSlow)
			default:
				continue
			}
			return
		}
	}
}

func (w *watched) stop() { w.stopper.Do(func() { close(w.done) }) }

// capReader fails once more than max bytes have been read.
type capReader struct {
	r    io.Reader
	max  int64
	n    int64
	over bool
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		c.over = true
		return n, errTooLarge
	}
	return n, err
}

// capBuffer keeps up to max bytes and notes when more were written.
type capBuffer struct {
	bytes.Buffer
	max  int64
	over bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if !c.over && int64(c.Len()+len(p)) <= c.max {
		c.Buffer.Write(p)
	} else {
		c.over = true
	}
	return len(p), nil
}
