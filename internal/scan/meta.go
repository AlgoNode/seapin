package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/types"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	mh "github.com/multiformats/go-multihash"
	"github.com/prometheus/client_golang/prometheus"
)

// MetaWriter keeps x_asset_meta, which Nodely's GraphQL serves as nft_meta,
// in the shape algoipfssvc wrote it, built from what seapin resolved for each
// asset (/internal/assets). It waits for an asset's objects to settle,
// retrying with backoff, and writes what it has after maxMetaTries.
type MetaWriter struct {
	DB        *pgxpool.Pool // scanner state
	Target    *pgxpool.Pool // the database holding x_asset_meta
	Table     string        // target table, [schema.]name; "" = x_asset_meta (a shadow copy for a parallel run)
	Chain     string
	SeapinURL string // seapin's internal listener
	Token     string
	Client    *http.Client
	Log       *slog.Logger

	written prometheus.Counter
}

const maxMetaTries = 8

func (m *MetaWriter) Register(reg prometheus.Registerer) {
	m.written = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "seapin_scan_meta_written_total", Help: "x_asset_meta rows written.",
	})
	reg.MustRegister(m.written)
}

func (m *MetaWriter) Run(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		if err := m.pass(ctx); err != nil && ctx.Err() == nil {
			m.Log.Warn("x_asset_meta pass", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

type dueAsset struct {
	id              int64
	name, unit, url string
	reserve         []byte
	refRound, tries int64
}

func (m *MetaWriter) pass(ctx context.Context) error {
	rows, err := m.DB.Query(ctx, `
		SELECT asset, COALESCE(name, ''), COALESCE(unit, ''), COALESCE(url, ''), reserve_addr, ref_round, meta_tries
		FROM scan_assets
		WHERE chain = $1 AND ref_round IS NOT NULL AND meta_round IS DISTINCT FROM ref_round
		  AND (meta_next_at IS NULL OR meta_next_at <= now())
		ORDER BY meta_next_at NULLS FIRST LIMIT 200`, m.Chain)
	if err != nil {
		return err
	}
	var due []dueAsset
	var d dueAsset
	if _, err := pgx.ForEachRow(rows, []any{&d.id, &d.name, &d.unit, &d.url, &d.reserve, &d.refRound, &d.tries}, func() error {
		due = append(due, d)
		return nil
	}); err != nil {
		return err
	}
	for _, a := range due {
		if ctx.Err() != nil {
			return nil
		}
		if err := m.one(ctx, a); err != nil {
			m.Log.Warn("x_asset_meta", "asset", a.id, "err", err)
		}
	}
	return nil
}

type resolvedObject struct {
	Key     string          `json:"key"`
	Root    string          `json:"root"`
	Subpath string          `json:"subpath"`
	CID     string          `json:"cid"`
	Status  string          `json:"status"`
	Mime    string          `json:"mime"`
	Size    int64           `json:"size"`
	Content json.RawMessage `json:"content"`
}

func (m *MetaWriter) one(ctx context.Context, a dueAsset) error {
	objs, err := m.resolved(ctx, a.id)
	if err != nil {
		return m.later(ctx, a, err)
	}
	settled := true
	for _, o := range objs {
		if o.Status == "" {
			settled = false
		}
	}
	if !settled && a.tries+1 < maxMetaTries {
		return m.later(ctx, a, nil)
	}
	meta, err := json.Marshal(buildMeta(a, objs))
	if err != nil {
		return err
	}
	if _, err := m.Target.Exec(ctx, `
		INSERT INTO `+m.table()+` (asset, meta, updated_at, cached_at) VALUES ($1, $2, now(), now())
		ON CONFLICT (asset) DO UPDATE SET meta = EXCLUDED.meta, updated_at = now(), cached_at = now()`,
		a.id, meta); err != nil {
		return err
	}
	m.written.Inc()
	_, err = m.DB.Exec(ctx, `
		UPDATE scan_assets SET meta_round = $3, meta_tries = 0, meta_next_at = NULL
		WHERE chain = $1 AND asset = $2`, m.Chain, a.id, a.refRound)
	return err
}

// table is the quoted target table (config.LoadScan validates META_TABLE).
func (m *MetaWriter) table() string {
	if m.Table == "" {
		return "x_asset_meta"
	}
	return pgx.Identifier(strings.Split(m.Table, ".")).Sanitize()
}

// later retries an asset with backoff: 30 s, 1 min, 2 min, … up to 1 h.
func (m *MetaWriter) later(ctx context.Context, a dueAsset, cause error) error {
	wait := min(30*time.Second<<a.tries, time.Hour)
	_, err := m.DB.Exec(ctx, `
		UPDATE scan_assets SET meta_tries = meta_tries + 1, meta_next_at = now() + make_interval(secs => $3)
		WHERE chain = $1 AND asset = $2`, m.Chain, a.id, wait.Seconds())
	if err == nil {
		err = cause
	}
	return err
}

func (m *MetaWriter) resolved(ctx context.Context, id int64) ([]resolvedObject, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/internal/assets/%s/%d", strings.TrimRight(m.SeapinURL, "/"), m.Chain, id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.Token)
	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("seapin: %s", resp.Status)
	}
	var out struct {
		Objects []resolvedObject `json:"objects"`
	}
	return out.Objects, json.NewDecoder(resp.Body).Decode(&out)
}

// The x_asset_meta JSON, as algoipfssvc's AssetInfo/ASAextra/ObjInfo wrote it.
type metaObject struct {
	MimeType string          `json:"mimetype,omitempty"`
	Bytes    int64           `json:"bytes,omitempty"`
	URL      string          `json:"url,omitempty"`
	CID      string          `json:"cid,omitempty"`
	Content  json.RawMessage `json:"content,omitempty"`
	Type     string          `json:"type,omitempty"`
}

type assetMeta struct {
	ID       int64                  `json:"id"`
	Name     string                 `json:"an,omitempty"`
	Reserve  string                 `json:"r,omitempty"`
	UnitName string                 `json:"un,omitempty"`
	URL      string                 `json:"url,omitempty"`
	Round    int64                  `json:"rnd,omitempty"`
	Extra    map[string]*metaObject `json:"extra,omitempty"`
}

// buildMeta keeps one object per media class as the old scanner did: links in
// its order (the asset URL, then the JSON's image, animation_url,
// properties.file_url), a later one replacing an earlier one of the same class
// (its ASAextra held one object per class). Cached objects and ones the policy
// refused count, the latter under their detected type (the old scanner recorded
// those too, e.g. text/plain under oth). CIDs and paths keep the spelling the
// asset used: seapin normalises to CIDv1 and unescaped paths, the old rows hold
// what the URL or the JSON said (v0 Qm…, %20) or what an ARC-19 template asked for.
func buildMeta(a dueAsset, objs []resolvedObject) assetMeta {
	written := []string{a.url}
	for _, o := range objs {
		if len(o.Content) > 0 {
			written = append(written, string(o.Content))
		}
	}
	v0Template := strings.Contains(a.url, "{ipfscid:0:")
	objs = slices.Clone(objs)
	slices.SortStableFunc(objs, func(x, y resolvedObject) int { return linkOrder(x.Key) - linkOrder(y.Key) })
	m := assetMeta{ID: a.id, Name: a.name, UnitName: a.unit, URL: a.url, Round: a.refRound, Extra: map[string]*metaObject{}}
	if len(a.reserve) == 32 {
		m.Reserve = types.Address(a.reserve).String()
	}
	for _, o := range objs {
		if o.Status != "cached" && o.Status != "blocked" {
			continue
		}
		class := mediaClass(o.Mime)
		w := written
		if v0Template && strings.HasSuffix(o.Key, "/url") {
			w = append(w, v0Of(o.Root)) // the template's own CID is v0
		}
		cid := spelled(o.Root, w)
		if o.Subpath != "" {
			cid += "/" + spelledPath(cid, o.Subpath, written)
		}
		mo := &metaObject{MimeType: o.Mime, Bytes: o.Size, CID: cid, Content: o.Content}
		if o.Status == "blocked" {
			mo.Content = nil
		}
		if class == "json" && len(o.Content) > 0 {
			var head struct {
				Image      *string `json:"image"`
				NFDAccount *string `json:"nfdAccount"`
			}
			if json.Unmarshal(o.Content, &head) == nil {
				switch {
				case head.NFDAccount != nil:
					mo.Type = "NFD"
				case head.Image != nil:
					mo.Type = "ARC-3"
				}
			}
		}
		m.Extra[class] = mo
	}
	webLinks(a, objs, m.Extra)
	if len(m.Extra) == 0 {
		m.Extra = nil
	}
	return m
}

// webLinks records links to plain web URLs (not IPFS) as {url} only, never
// fetched (operator 2026-11-03; the old scanner fetched them, an SSRF surface):
// the asset URL itself, and the media fields of its JSON metadata (NFD avatars on
// images.nf.domains). Only where seapin resolved no object for that link, and
// never over an object of the same class.
func webLinks(a dueAsset, objs []resolvedObject, extra map[string]*metaObject) {
	have := map[string]bool{}
	for _, o := range objs {
		have[o.Key] = true
	}
	urlKey := ""
	for _, o := range objs { // the asset URL's own object key, e.g. mainnet-v1.0/<asset>/url
		if strings.HasSuffix(o.Key, "/url") {
			urlKey = o.Key
		}
	}
	type link struct{ path, url, class string }
	var links []link
	if isWeb(a.url) && urlKey == "" {
		links = append(links, link{"url", a.url, "img"})
	}
	for _, o := range objs {
		if mediaClass(o.Mime) != "json" || len(o.Content) == 0 {
			continue
		}
		var j struct {
			Image      string `json:"image"`
			Animation  string `json:"animation_url"`
			Properties struct {
				FileURL string `json:"file_url"`
			} `json:"properties"`
		}
		if json.Unmarshal(o.Content, &j) != nil {
			continue
		}
		for _, l := range []link{{"image", j.Image, "img"}, {"animation_url", j.Animation, "vid"}, {"properties/file_url", j.Properties.FileURL, "oth"}} {
			if isWeb(l.url) && !have[o.Key+"/"+l.path] {
				links = append(links, l)
			}
		}
	}
	for _, l := range links {
		class := l.class
		if c := classByExt(l.url); c != "" {
			class = c
		}
		if extra[class] == nil {
			extra[class] = &metaObject{URL: l.url}
		}
	}
}

// isWeb: an http(s) URL that is not an IPFS gateway path (those seapin resolves itself).
func isWeb(u string) bool {
	l := strings.ToLower(u)
	return (strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://")) && !strings.Contains(l, "/ipfs/") &&
		!strings.Contains(l, ".ipfs.")
}

func classByExt(u string) string {
	p := strings.ToLower(u)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	switch {
	case strings.HasSuffix(p, ".png"), strings.HasSuffix(p, ".jpg"), strings.HasSuffix(p, ".jpeg"), strings.HasSuffix(p, ".gif"),
		strings.HasSuffix(p, ".webp"), strings.HasSuffix(p, ".svg"), strings.HasSuffix(p, ".avif"):
		return "img"
	case strings.HasSuffix(p, ".mp4"), strings.HasSuffix(p, ".webm"), strings.HasSuffix(p, ".mov"):
		return "vid"
	case strings.HasSuffix(p, ".mp3"), strings.HasSuffix(p, ".wav"), strings.HasSuffix(p, ".ogg"), strings.HasSuffix(p, ".flac"):
		return "aud"
	case strings.HasSuffix(p, ".json"):
		return "json"
	}
	return ""
}

// linkOrder ranks an object by the old scanner's order of links: the asset URL's
// own object, then image, animation_url, properties.file_url (a later one wins).
func linkOrder(key string) int {
	switch {
	case strings.HasSuffix(key, "/url"):
		return 0
	case strings.HasSuffix(key, "/image"):
		return 1
	case strings.HasSuffix(key, "/animation_url"):
		return 2
	case strings.HasSuffix(key, "/properties/file_url"):
		return 3
	}
	return 1
}

// spelledPath returns the subpath escaped as the asset wrote it after root
// (ZombiePuddin%20%23321.png), else as seapin holds it.
func spelledPath(root, sub string, written []string) string {
	segs := strings.Split(sub, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	esc := strings.Join(segs, "/")
	if esc == sub {
		return sub
	}
	for _, w := range written {
		if strings.Contains(w, root+"/"+esc) {
			return esc
		}
	}
	return sub
}

// v0Of is norm's CIDv0 form, or "" when it has none.
func v0Of(norm string) string {
	c, err := cid.Decode(norm)
	if err != nil || c.Type() != cid.DagProtobuf {
		return ""
	}
	if dec, err := mh.Decode(c.Hash()); err != nil || dec.Code != mh.SHA2_256 || dec.Length != 32 {
		return ""
	}
	return cid.NewCidV0(c.Hash()).String()
}

// spelled returns norm as the asset wrote it: the CIDv0 form when norm is a
// dag-pb sha2-256 CIDv1 whose v0 string appears in the asset's URL or JSON,
// else norm.
func spelled(norm string, written []string) string {
	c, err := cid.Decode(norm)
	if err != nil || c.Version() != 1 || c.Type() != cid.DagProtobuf {
		return norm
	}
	dec, err := mh.Decode(c.Hash())
	if err != nil || dec.Code != mh.SHA2_256 || dec.Length != 32 {
		return norm
	}
	v0 := cid.NewCidV0(c.Hash()).String()
	for _, w := range written {
		if w == "" {
			continue
		}
		if strings.Contains(w, v0) {
			return v0
		}
	}
	return norm
}

func mediaClass(mime string) string {
	switch {
	case strings.Contains(mime, "json"):
		return "json"
	case strings.HasPrefix(mime, "image/"):
		return "img"
	case strings.HasPrefix(mime, "video/"):
		return "vid"
	case strings.HasPrefix(mime, "audio/"):
		return "aud"
	}
	return "oth"
}
