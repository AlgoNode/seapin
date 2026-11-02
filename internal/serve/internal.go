package serve

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"seapin/internal/catalog"
	"seapin/internal/cidkey"
)

// maxInlineJSON is the largest JSON object /internal/assets returns inline,
// matching x_asset_meta's "content" (the old scanner kept 10 KiB).
const maxInlineJSON = 10 << 10

// InternalRoutes is the listener for our own services only (tailnet, bearer
// token): the other region fetches cached objects through /internal/ipfs/,
// the scanner reads what an asset resolved to through /internal/assets/, and
// /internal/cids/{cid}/assets answers the reverse: which assets reach a CID.
func (h *Handler) InternalRoutes(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/ipfs/{cid}", h.ipfs)
	mux.HandleFunc("GET /internal/ipfs/{cid}/{path...}", h.ipfs)
	mux.HandleFunc("GET /internal/assets/{chain}/{asset}", h.asset)
	mux.HandleFunc("GET /internal/cids/{cid}/assets", h.cidAssets)
	if h.Sync != nil {
		mux.HandleFunc("GET /internal/catalog/{kind}", h.catalogPage)
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Header.Set(peerHeader, "1") // never proxy a peer's request on to the peer
		mux.ServeHTTP(w, r)
	})
}

type assetObject struct {
	Key     string          `json:"key"`
	Root    string          `json:"root"`
	Subpath string          `json:"subpath,omitempty"`
	CID     string          `json:"cid,omitempty"` // leaf; empty until fetched
	Depth   int             `json:"depth"`
	Status  string          `json:"status,omitempty"`
	Mime    string          `json:"mime,omitempty"`
	Size    int64           `json:"size,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	Content json.RawMessage `json:"content,omitempty"` // cached JSON up to 10 KiB
}

func (h *Handler) asset(w http.ResponseWriter, r *http.Request) {
	chain := r.PathValue("chain")
	asset, err := strconv.ParseUint(r.PathValue("asset"), 10, 64)
	if err != nil || asset == 0 {
		http.Error(w, "invalid asset", http.StatusBadRequest)
		return
	}
	objs, err := h.Catalog.AssetObjects(r.Context(), chain, asset)
	if err != nil {
		h.Log.Error("asset objects", "chain", chain, "asset", asset, "err", err)
		http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	out := struct {
		Chain   string        `json:"chain"`
		Asset   uint64        `json:"asset"`
		Objects []assetObject `json:"objects"`
	}{Chain: chain, Asset: asset, Objects: []assetObject{}}
	for _, o := range objs {
		ao := assetObject{
			Key: o.Key, Root: o.Root, Subpath: o.Subpath, CID: o.Leaf, Depth: o.Depth,
			Status: o.Status, Mime: o.Mime, Size: o.Size, Reason: o.Reason,
		}
		if o.Status == string(catalog.StatusCached) && o.Mime == "application/json" && o.Size <= maxInlineJSON {
			ao.Content = h.readJSON(r, o.Leaf)
		}
		out.Objects = append(out.Objects, ao)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

func (h *Handler) readJSON(r *http.Request, leaf string) json.RawMessage {
	body, err := h.Store.GetRange(r.Context(), cidkey.ObjectKey(leaf), 0)
	if err != nil {
		return nil
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, maxInlineJSON+1))
	if err != nil || len(b) > maxInlineJSON || !json.Valid(b) {
		return nil
	}
	return b
}

// maxCIDAssets caps /internal/cids/{cid}/assets; a shared default image can be
// reached by thousands of assets.
const maxCIDAssets = 1000

func (h *Handler) cidAssets(w http.ResponseWriter, r *http.Request) {
	id, err := cidkey.Normalize(r.PathValue("cid"))
	if err != nil {
		http.Error(w, "invalid CID", http.StatusBadRequest)
		return
	}
	chain := r.URL.Query().Get("chain")
	if chain == "" {
		chain = "mainnet-v1.0"
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, maxCIDAssets)
		}
	}
	// one more than asked tells whether the list was cut
	as, err := h.Catalog.CIDAssets(r.Context(), chain, id, limit+1)
	if err != nil {
		h.Log.Error("cid assets", "cid", id, "err", err)
		http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	out := struct {
		CID       string             `json:"cid"`
		Chain     string             `json:"chain"`
		Assets    []catalog.CIDAsset `json:"assets"`
		Truncated bool               `json:"truncated"`
	}{CID: id, Chain: chain, Assets: []catalog.CIDAsset{}}
	if len(as) > limit {
		as, out.Truncated = as[:limit], true
	}
	out.Assets = append(out.Assets, as...)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// maxSyncPage caps one anti-entropy page.
const maxSyncPage = 5000

// catalogPage serves one page of this region's catalog to the peer's
// anti-entropy pull: refs and objects since a time (RFC 3339, nanoseconds)
// after a key, aliases after a (root, subpath) pair, or every takedown.
func (h *Handler) catalogPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 1000
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, maxSyncPage)
	}
	since := time.Unix(0, 0).UTC()
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			http.Error(w, "since: RFC 3339", http.StatusBadRequest)
			return
		}
		since = t
	}
	var out any
	var err error
	switch r.PathValue("kind") {
	case "refs":
		out, err = h.Sync.RefsPage(r.Context(), since, q.Get("after"), limit)
	case "objects":
		out, err = h.Sync.ObjectsPage(r.Context(), since, q.Get("after"), limit)
	case "aliases":
		out, err = h.Sync.AliasesPage(r.Context(), q.Get("after"), q.Get("after_subpath"), limit)
	case "takedowns":
		out, err = h.Sync.TakedownList(r.Context())
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.Log.Error("catalog page", "kind", r.PathValue("kind"), "err", err)
		http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}
