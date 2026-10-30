// Package catalog is the region's Postgres record of every object seapin knows:
// its status, the chain references that point at it, path aliases and
// takedowns. Nothing in it is ever deleted (the cache is append-only).
package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusCached  Status = "cached"
	StatusBlocked Status = "blocked"
	StatusFailed  Status = "failed"
)

// Object is one leaf CID's catalog row, as far as serving needs it.
type Object struct {
	CID    string
	Status Status
	Mime   string
	Size   int64
	Reason string
}

type Catalog struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*Catalog, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return &Catalog{pool: pool}, nil
}

func (c *Catalog) Close() { c.pool.Close() }

func (c *Catalog) Ping(ctx context.Context) error { return c.pool.Ping(ctx) }

// Lookup returns the object's row; found is false when the CID is unknown.
func (c *Catalog) Lookup(ctx context.Context, cid string) (o Object, found bool, err error) {
	var mime, reason *string
	var size *int64
	err = c.pool.QueryRow(ctx,
		`SELECT cid, status, mime, size, reason FROM objects WHERE cid = $1`, cid,
	).Scan(&o.CID, &o.Status, &mime, &size, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return Object{}, false, nil
	}
	if err != nil {
		return Object{}, false, err
	}
	o.Mime, o.Reason = deref(mime), deref(reason)
	if size != nil {
		o.Size = *size
	}
	return o, true, nil
}

// ResolveAlias maps a path inside a root CID (a UnixFS directory) to the leaf
// CID stored for it.
func (c *Catalog) ResolveAlias(ctx context.Context, root, subpath string) (leaf string, found bool, err error) {
	err = c.pool.QueryRow(ctx,
		`SELECT leaf_cid FROM aliases WHERE root_cid = $1 AND subpath = $2`, root, subpath,
	).Scan(&leaf)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return leaf, err == nil, err
}

// Takedowns returns every CID an operator has taken down, with the reason.
func (c *Catalog) Takedowns(ctx context.Context) (map[string]string, error) {
	rows, err := c.pool.Query(ctx, `SELECT cid, reason FROM takedowns`)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	var cid, reason string
	_, err = pgx.ForEachRow(rows, []any{&cid, &reason}, func() error {
		m[cid] = reason
		return nil
	})
	return m, err
}

// MarkCached records that an object is stored. first_seen_at keeps its first value.
func (c *Catalog) MarkCached(ctx context.Context, cid, mime string, size int64, source string) error {
	_, err := c.pool.Exec(ctx, `
		INSERT INTO objects (cid, status, mime, size, cached_at, source)
		VALUES ($1, 'cached', $2, $3, now(), $4)
		ON CONFLICT (cid) DO UPDATE
		SET status = 'cached', mime = $2, size = $3, reason = NULL,
		    cached_at = now(), source = $4, lease_until = NULL`,
		cid, mime, size, source)
	return err
}

// MarkBlocked records a policy refusal. It never downgrades an object that is
// already cached.
func (c *Catalog) MarkBlocked(ctx context.Context, cid, detected string, size int64, reason string) error {
	_, err := c.pool.Exec(ctx, `
		INSERT INTO objects (cid, status, mime, size, reason)
		VALUES ($1, 'blocked', $2, $3, $4)
		ON CONFLICT (cid) DO UPDATE
		SET status = 'blocked', mime = $2, size = $3, reason = $4, lease_until = NULL
		WHERE objects.status <> 'cached'`,
		cid, detected, size, reason)
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
