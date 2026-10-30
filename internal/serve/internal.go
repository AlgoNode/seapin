package serve

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"seapin/internal/catalog"
	"seapin/internal/cidkey"
)

// maxInlineJSON is the largest JSON object /internal/assets returns inline,
// matching x_asset_meta's "content" (the old scanner kept 10 KiB).
const maxInlineJSON = 10 << 10

// InternalRoutes is the listener for our own services only (tailnet, bearer
// token): the other region fetches cached objects through /internal/ipfs/,
// and the scanner reads what an asset resolved to through /internal/assets/.
func (h *Handler) InternalRoutes(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/ipfs/{cid}", h.ipfs)
	mux.HandleFunc("GET /internal/ipfs/{cid}/{path...}", h.ipfs)
	mux.HandleFunc("GET /internal/assets/{chain}/{asset}", h.asset)
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
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
