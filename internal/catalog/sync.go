package catalog

import (
	"context"
	"time"

	"seapin/internal/contract"
)

// Anti-entropy (PLAN §2): the peer serves pages of its catalog, this region
// keeps what it lacks. Pages are keyset-ordered so a pull resumes exactly.

// SyncRef is one reference as the peer serves it, with its update time.
type SyncRef struct {
	contract.Ref
	UpdatedAt time.Time `json:"updated_at"`
}

// SyncObject is one cached object as the peer serves it.
type SyncObject struct {
	CID       string    `json:"cid"`
	CachedAt  time.Time `json:"cached_at"`
	FirstSeen time.Time `json:"first_seen_at"`
}

// SyncAlias is one directory path the peer resolved.
type SyncAlias struct {
	Root    string `json:"root"`
	Subpath string `json:"subpath"`
	Leaf    string `json:"leaf"`
}

// RefsPage returns references updated at or after since, after (since, after) in (updated_at, key) order.
func (c *Catalog) RefsPage(ctx context.Context, since time.Time, after string, limit int) ([]SyncRef, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT key, chain, asset, path, root_cid, subpath, coalesce(creator, ''), round, coalesce(txid, ''),
		       coalesce(block_time, 'epoch'), updated_at
		FROM refs WHERE (updated_at, key) > ($1, $2) ORDER BY updated_at, key LIMIT $3`, since, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncRef
	for rows.Next() {
		var r SyncRef
		var asset, round int64
		if err := rows.Scan(&r.Key, &r.Chain, &asset, &r.Path, &r.CID, &r.Subpath, &r.Creator, &round, &r.TxID,
			&r.BlockTime, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.V, r.Type, r.Asset, r.Round, r.Src = contract.Version, contract.TypeRef, uint64(asset), uint64(round), contract.SrcBackfill
		out = append(out, r)
	}
	return out, rows.Err()
}

// ObjectsPage returns cached objects in (cached_at, cid) order after (since, after).
func (c *Catalog) ObjectsPage(ctx context.Context, since time.Time, after string, limit int) ([]SyncObject, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT cid, cached_at, first_seen_at FROM objects
		WHERE status = 'cached' AND (cached_at, cid) > ($1, $2) ORDER BY cached_at, cid LIMIT $3`, since, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncObject
	for rows.Next() {
		var o SyncObject
		if err := rows.Scan(&o.CID, &o.CachedAt, &o.FirstSeen); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// AliasesPage returns aliases in (root, subpath) order after the given pair.
func (c *Catalog) AliasesPage(ctx context.Context, afterRoot, afterSub string, limit int) ([]SyncAlias, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT root_cid, subpath, leaf_cid FROM aliases WHERE (root_cid, subpath) > ($1, $2)
		ORDER BY root_cid, subpath LIMIT $3`, afterRoot, afterSub, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncAlias
	for rows.Next() {
		var a SyncAlias
		if err := rows.Scan(&a.Root, &a.Subpath, &a.Leaf); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// StaleRefs returns the refs of the list this catalog lacks or holds at an older round.
func (c *Catalog) StaleRefs(ctx context.Context, refs []SyncRef) ([]SyncRef, error) {
	keys := make([]string, len(refs))
	for i, r := range refs {
		keys[i] = r.Key
	}
	have := map[string]int64{}
	rows, err := c.pool.Query(ctx, `SELECT key, round FROM refs WHERE key = ANY($1)`, keys)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var r int64
		if err := rows.Scan(&k, &r); err != nil {
			rows.Close()
			return nil, err
		}
		have[k] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []SyncRef
	for _, r := range refs {
		if round, ok := have[r.Key]; !ok || round < int64(r.Round) {
			out = append(out, r)
		}
	}
	return out, nil
}

// MissingObjects returns the CIDs of the list that are not cached here.
func (c *Catalog) MissingObjects(ctx context.Context, cids []string) ([]string, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT x FROM unnest($1::text[]) x
		WHERE NOT EXISTS (SELECT 1 FROM objects o WHERE o.cid = x AND o.status IN ('cached', 'blocked'))`, cids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MissingAliases returns the aliases of the list this catalog has not resolved.
func (c *Catalog) MissingAliases(ctx context.Context, as []SyncAlias) ([]SyncAlias, error) {
	roots, subs := make([]string, len(as)), make([]string, len(as))
	for i, a := range as {
		roots[i], subs[i] = a.Root, a.Subpath
	}
	have := map[[2]string]bool{}
	rows, err := c.pool.Query(ctx, `
		SELECT a.root_cid, a.subpath FROM aliases a
		JOIN unnest($1::text[], $2::text[]) u(r, s) ON a.root_cid = u.r AND a.subpath = u.s`, roots, subs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r, s string
		if err := rows.Scan(&r, &s); err != nil {
			rows.Close()
			return nil, err
		}
		have[[2]string{r, s}] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []SyncAlias
	for _, a := range as {
		if !have[[2]string{a.Root, a.Subpath}] {
			out = append(out, a)
		}
	}
	return out, nil
}

// EnqueueFetch queues root/subpath in the backfill lane under creator, as
// ingest does for a reference (an already known leaf queues nothing).
func (c *Catalog) EnqueueFetch(ctx context.Context, root, subpath, creator string, firstSeen time.Time) (int64, error) {
	return enqueue(ctx, c.pool, fetchReq{root: root, subpath: subpath, lane: LaneBackfill, creator: creator, firstSeen: firstSeen})
}

// SyncState is where the last pull of kind stopped.
func (c *Catalog) SyncState(ctx context.Context, kind string) (since time.Time, after string, err error) {
	err = c.pool.QueryRow(ctx, `
		INSERT INTO peer_sync (kind) VALUES ($1) ON CONFLICT (kind) DO UPDATE SET kind = EXCLUDED.kind
		RETURNING since, after_key`, kind).Scan(&since, &after)
	return since, after, err
}

// SaveSyncState records where the pull of kind stopped.
func (c *Catalog) SaveSyncState(ctx context.Context, kind string, since time.Time, after string) error {
	_, err := c.pool.Exec(ctx, `
		INSERT INTO peer_sync (kind, since, after_key, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (kind) DO UPDATE SET since = EXCLUDED.since, after_key = EXCLUDED.after_key, updated_at = now()`,
		kind, since, after)
	return err
}

// TakedownList returns every takedown (a short list).
func (c *Catalog) TakedownList(ctx context.Context) (map[string]string, error) {
	return c.Takedowns(ctx)
}
