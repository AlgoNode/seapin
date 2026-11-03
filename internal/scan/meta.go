package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// buildMeta keeps the first cached object of each media class, in key order
// (the URL's own object first), as the old scanner did.
func buildMeta(a dueAsset, objs []resolvedObject) assetMeta {
	m := assetMeta{ID: a.id, Name: a.name, UnitName: a.unit, URL: a.url, Round: a.refRound, Extra: map[string]*metaObject{}}
	if len(a.reserve) == 32 {
		m.Reserve = types.Address(a.reserve).String()
	}
	for _, o := range objs {
		if o.Status != "cached" {
			continue
		}
		class := mediaClass(o.Mime)
		if m.Extra[class] != nil {
			continue
		}
		cid := o.Root
		if o.Subpath != "" {
			cid += "/" + o.Subpath
		}
		mo := &metaObject{MimeType: o.Mime, Bytes: o.Size, CID: cid, Content: o.Content}
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
	if len(m.Extra) == 0 {
		m.Extra = nil
	}
	return m
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
