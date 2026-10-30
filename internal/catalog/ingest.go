package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"seapin/internal/contract"
)

const (
	LaneFast     = "fast"
	LaneSlow     = "slow"
	LaneBackfill = "backfill"

	// MaxDepth bounds how many JSON hops ingest follows from a chain-level ref.
	MaxDepth = 3
)

// querier is what both the pool and a transaction offer.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Job is a claimed fetch: fetch Subpath inside Root.
type Job struct {
	Root, Subpath string
	Lane, Creator string
	Depth         int
	Attempts      int // including this one
	FirstSeen     time.Time
}

// Link is an IPFS link found inside a stored JSON object.
type Link struct {
	Path, CID, Subpath string
}

// Outcome is what a fetch produced: the leaf object it resolved to and
// whether it is stored (cached) or refused (blocked).
type Outcome struct {
	Leaf   string
	Status Status // StatusCached or StatusBlocked
	Mime   string // served type when cached, detected type when blocked
	Size   int64
	Reason string
	Source string
	Links  []Link // links inside a cached JSON object
}

type fetchReq struct {
	root, subpath string
	lane, creator string
	depth         int
	firstSeen     time.Time
}

// ApplyRef records a chain-level reference (the newest round per key wins)
// and queues what it points at. A ref older than the stored one changes
// nothing: superseded ARC-19 versions are not fetched.
func (c *Catalog) ApplyRef(ctx context.Context, r contract.Ref) (queued int64, err error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var written bool
	err = tx.QueryRow(ctx, `
		INSERT INTO refs (key, chain, asset, path, root_cid, subpath, creator, round, txid, block_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (key) DO UPDATE
		SET root_cid = EXCLUDED.root_cid, subpath = EXCLUDED.subpath, creator = EXCLUDED.creator,
		    round = EXCLUDED.round, txid = EXCLUDED.txid, block_time = EXCLUDED.block_time, updated_at = now()
		WHERE refs.round <= EXCLUDED.round
		RETURNING true`,
		r.Key, r.Chain, int64(r.Asset), r.Path, r.CID, r.Subpath, r.Creator, int64(r.Round), r.TxID, r.BlockTime,
	).Scan(&written)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, tx.Commit(ctx)
	}
	if err != nil {
		return 0, err
	}
	lane := LaneFast
	if r.Src == contract.SrcBackfill {
		lane = LaneBackfill
	}
	queued, err = enqueue(ctx, tx, fetchReq{
		root: r.CID, subpath: r.Subpath, lane: lane, creator: r.Creator, firstSeen: r.BlockTime,
	})
	if err != nil {
		return 0, err
	}
	return queued, tx.Commit(ctx)
}

// enqueue queues a fetch unless its leaf is already known, in which case it
// makes sure the leaf's JSON children are queued instead. A live request
// promotes a waiting backfill fetch to the fast lane; a failed fetch is
// re-armed at most once per 30 days.
func enqueue(ctx context.Context, q querier, f fetchReq) (int64, error) {
	leaf, err := knownLeaf(ctx, q, f.root, f.subpath)
	if err != nil {
		return 0, err
	}
	if leaf != "" {
		return enqueueLinked(ctx, q, leaf, f)
	}
	tag, err := q.Exec(ctx, `
		INSERT INTO fetches (root_cid, subpath, lane, creator, depth, first_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (root_cid, subpath) DO UPDATE SET
		    status      = 'pending',
		    lane        = CASE WHEN fetches.status = 'failed' THEN EXCLUDED.lane ELSE 'fast' END,
		    attempts    = CASE WHEN fetches.status = 'failed' THEN 0 ELSE fetches.attempts END,
		    rearmed_at  = CASE WHEN fetches.status = 'failed' THEN now() ELSE fetches.rearmed_at END,
		    depth       = LEAST(fetches.depth, EXCLUDED.depth),
		    next_try_at = now(),
		    updated_at  = now()
		WHERE (fetches.status = 'failed'
		       AND (fetches.rearmed_at IS NULL OR fetches.rearmed_at < now() - interval '30 days'))
		   OR (fetches.status = 'pending' AND fetches.lane = 'backfill' AND EXCLUDED.lane = 'fast'
		       AND (fetches.lease_until IS NULL OR fetches.lease_until < now()))`,
		f.root, f.subpath, f.lane, f.creator, f.depth, f.firstSeen)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// knownLeaf is the leaf CID already recorded for root/subpath, or "".
func knownLeaf(ctx context.Context, q querier, root, subpath string) (string, error) {
	var leaf string
	var err error
	if subpath == "" {
		err = q.QueryRow(ctx, `SELECT cid FROM objects WHERE cid = $1`, root).Scan(&leaf)
	} else {
		err = q.QueryRow(ctx, `SELECT leaf_cid FROM aliases WHERE root_cid = $1 AND subpath = $2`, root, subpath).Scan(&leaf)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return leaf, err
}

// enqueueLinked queues the not-yet-known children of a known JSON leaf, and
// walks on through children that are known, down to MaxDepth. Children
// inherit the parent's creator; backfill stays backfill, the rest go fast.
func enqueueLinked(ctx context.Context, q querier, leaf string, parent fetchReq) (int64, error) {
	lane := LaneFast
	if parent.lane == LaneBackfill {
		lane = LaneBackfill
	}
	var queued int64
	frontier := []string{leaf}
	for depth := parent.depth + 1; depth <= MaxDepth && len(frontier) > 0; depth++ {
		rows, err := q.Query(ctx, `
			SELECT l.child_cid, l.subpath, COALESCE(CASE WHEN l.subpath = '' THEN o.cid ELSE a.leaf_cid END, '')
			FROM links l
			LEFT JOIN objects o ON l.subpath = '' AND o.cid = l.child_cid
			LEFT JOIN aliases a ON l.subpath <> '' AND a.root_cid = l.child_cid AND a.subpath = l.subpath
			WHERE l.parent_cid = ANY($1)`, frontier)
		if err != nil {
			return queued, err
		}
		type child struct{ cid, subpath, leaf string }
		var children []child
		var ch child
		_, err = pgx.ForEachRow(rows, []any{&ch.cid, &ch.subpath, &ch.leaf}, func() error {
			children = append(children, ch)
			return nil
		})
		if err != nil {
			return queued, err
		}
		frontier = frontier[:0]
		for _, ch := range children {
			if ch.leaf != "" {
				frontier = append(frontier, ch.leaf)
				continue
			}
			tag, err := q.Exec(ctx, `
				INSERT INTO fetches (root_cid, subpath, lane, creator, depth, first_seen_at)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (root_cid, subpath) DO NOTHING`,
				ch.cid, ch.subpath, lane, parent.creator, depth, parent.firstSeen)
			if err != nil {
				return queued, err
			}
			queued += tag.RowsAffected()
		}
	}
	return queued, nil
}

// Claim leases up to n due fetches of a lane. With fair set it takes at most
// one per creator per call (oldest first) and none from the excluded
// creators, so one creator's flood gets one share. One dispatcher per lane
// and region is assumed; the lease re-check guards against a second one.
func (c *Catalog) Claim(ctx context.Context, lane string, n int, exclude []string, lease time.Duration, fair bool) ([]Job, error) {
	pick := `
		SELECT root_cid, subpath FROM fetches
		WHERE status = 'pending' AND lane = $1 AND next_try_at <= now()
		  AND (lease_until IS NULL OR lease_until < now()) AND NOT (creator = ANY($3))
		ORDER BY next_try_at LIMIT $2
		FOR UPDATE SKIP LOCKED`
	if fair {
		pick = `
		WITH oldest AS (
		    SELECT DISTINCT ON (creator) root_cid, subpath, next_try_at FROM fetches
		    WHERE status = 'pending' AND lane = $1 AND next_try_at <= now()
		      AND (lease_until IS NULL OR lease_until < now()) AND NOT (creator = ANY($3))
		    ORDER BY creator, next_try_at
		)
		SELECT f.root_cid, f.subpath FROM fetches f
		JOIN oldest o ON o.root_cid = f.root_cid AND o.subpath = f.subpath
		ORDER BY o.next_try_at LIMIT $2
		FOR UPDATE OF f SKIP LOCKED`
	}
	if exclude == nil {
		exclude = []string{}
	}
	rows, err := c.pool.Query(ctx, `
		WITH pick AS (`+pick+`)
		UPDATE fetches f
		SET lease_until = now() + make_interval(secs => $4), attempts = f.attempts + 1, updated_at = now()
		FROM pick
		WHERE f.root_cid = pick.root_cid AND f.subpath = pick.subpath
		  AND (f.lease_until IS NULL OR f.lease_until < now())
		RETURNING f.root_cid, f.subpath, f.lane, f.creator, f.depth, f.attempts, f.first_seen_at`,
		lane, n, exclude, lease.Seconds())
	if err != nil {
		return nil, err
	}
	var jobs []Job
	var j Job
	_, err = pgx.ForEachRow(rows, []any{&j.Root, &j.Subpath, &j.Lane, &j.Creator, &j.Depth, &j.Attempts, &j.FirstSeen}, func() error {
		jobs = append(jobs, j)
		return nil
	})
	return jobs, err
}

// Complete records a finished fetch: the leaf object (never downgrading a
// cached one), the path alias, the links inside a JSON leaf, and the children
// those links lead to. It returns how many child fetches were queued.
func (c *Catalog) Complete(ctx context.Context, j Job, o Outcome) (int64, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO objects (cid, status, mime, size, reason, first_seen_at, cached_at, source)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, CASE WHEN $2 = 'cached' THEN now() END, NULLIF($7, ''))
		ON CONFLICT (cid) DO UPDATE SET
		    status = EXCLUDED.status, mime = EXCLUDED.mime, size = EXCLUDED.size, reason = EXCLUDED.reason,
		    cached_at = COALESCE(objects.cached_at, EXCLUDED.cached_at),
		    source = COALESCE(objects.source, EXCLUDED.source),
		    first_seen_at = LEAST(objects.first_seen_at, EXCLUDED.first_seen_at)
		WHERE objects.status <> 'cached'`,
		o.Leaf, string(o.Status), o.Mime, o.Size, o.Reason, j.FirstSeen, o.Source); err != nil {
		return 0, err
	}
	if j.Subpath != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO aliases (root_cid, subpath, leaf_cid) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, j.Root, j.Subpath, o.Leaf); err != nil {
			return 0, err
		}
	}
	for _, l := range o.Links {
		if _, err := tx.Exec(ctx, `
			INSERT INTO links (parent_cid, json_path, child_cid, subpath) VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, o.Leaf, l.Path, l.CID, l.Subpath); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE fetches SET status = 'done', leaf_cid = $3, lease_until = NULL, last_error = NULL, updated_at = now()
		WHERE root_cid = $1 AND subpath = $2`, j.Root, j.Subpath, o.Leaf); err != nil {
		return 0, err
	}
	queued, err := enqueueLinked(ctx, tx, o.Leaf, fetchReq{lane: j.Lane, creator: j.Creator, depth: j.Depth, firstSeen: j.FirstSeen})
	if err != nil {
		return 0, err
	}
	return queued, tx.Commit(ctx)
}

// Retry puts a fetch back for another attempt in lane at the given time.
func (c *Catalog) Retry(ctx context.Context, j Job, lane string, at time.Time, cause string) error {
	_, err := c.pool.Exec(ctx, `
		UPDATE fetches SET lane = $3, next_try_at = $4, lease_until = NULL, last_error = $5, updated_at = now()
		WHERE root_cid = $1 AND subpath = $2`, j.Root, j.Subpath, lane, at, cause)
	return err
}

// Fail gives up on a fetch until a new reference re-arms it.
func (c *Catalog) Fail(ctx context.Context, j Job, cause string) error {
	_, err := c.pool.Exec(ctx, `
		UPDATE fetches SET status = 'failed', lease_until = NULL, last_error = $3, updated_at = now()
		WHERE root_cid = $1 AND subpath = $2`, j.Root, j.Subpath, cause)
	return err
}

// AddTakedown records an operator block (the reason of a repeated one is updated).
func (c *Catalog) AddTakedown(ctx context.Context, cid, reason string) error {
	_, err := c.pool.Exec(ctx, `
		INSERT INTO takedowns (cid, reason) VALUES ($1, $2)
		ON CONFLICT (cid) DO UPDATE SET reason = EXCLUDED.reason`, cid, reason)
	return err
}

// LaneStat is the backlog of one lane.
type LaneStat struct {
	Pending   int64
	OldestDue time.Duration // how long the oldest due fetch has been waiting
}

func (c *Catalog) LaneStats(ctx context.Context) (map[string]LaneStat, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT lane, count(*),
		       COALESCE(EXTRACT(EPOCH FROM now() - min(next_try_at) FILTER (WHERE next_try_at <= now())), 0)
		FROM fetches WHERE status = 'pending' GROUP BY lane`)
	if err != nil {
		return nil, err
	}
	stats := map[string]LaneStat{LaneFast: {}, LaneSlow: {}, LaneBackfill: {}, LaneDemand: {}}
	var lane string
	var n int64
	var secs float64
	_, err = pgx.ForEachRow(rows, []any{&lane, &n, &secs}, func() error {
		stats[lane] = LaneStat{Pending: n, OldestDue: time.Duration(secs * float64(time.Second))}
		return nil
	})
	return stats, err
}

// AssetObject is one object an asset reaches (asset_objects()).
type AssetObject struct {
	Key, Root, Subpath, Leaf string
	Depth                    int
	Status, Mime, Reason     string
	Size                     int64
}

func (c *Catalog) AssetObjects(ctx context.Context, chain string, asset uint64) ([]AssetObject, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT key, root_cid, subpath, COALESCE(leaf_cid, ''), depth,
		       COALESCE(status, ''), COALESCE(mime, ''), COALESCE(reason, ''), COALESCE(size, 0)
		FROM asset_objects($1, $2)`, chain, int64(asset))
	if err != nil {
		return nil, err
	}
	var out []AssetObject
	var o AssetObject
	_, err = pgx.ForEachRow(rows, []any{&o.Key, &o.Root, &o.Subpath, &o.Leaf, &o.Depth, &o.Status, &o.Mime, &o.Reason, &o.Size}, func() error {
		out = append(out, o)
		return nil
	})
	return out, err
}
