package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	LaneDemand = "demand"

	// fetchChannel is the NOTIFY channel serve uses to wake ingest's lanes.
	fetchChannel = "seapin_fetch"
)

// FetchState is where the fetch of root/subpath stands.
type FetchState struct {
	Found   bool
	Status  string // pending | done | failed
	Lane    string
	Leaf    string // set when done
	NextTry time.Time
}

func (c *Catalog) FetchState(ctx context.Context, root, subpath string) (FetchState, error) {
	var s FetchState
	var leaf *string
	err := c.pool.QueryRow(ctx, `
		SELECT status, lane, leaf_cid, next_try_at FROM fetches WHERE root_cid = $1 AND subpath = $2`,
		root, subpath).Scan(&s.Status, &s.Lane, &leaf, &s.NextTry)
	if errors.Is(err, pgx.ErrNoRows) {
		return FetchState{}, nil
	}
	if err != nil {
		return FetchState{}, err
	}
	s.Found, s.Leaf = true, deref(leaf)
	return s, nil
}

// Demand queues a read-through fetch of root/subpath in the demand lane, with
// the referring site as its fair-share key, unless a fetch exists; a waiting
// backfill fetch moves to the demand lane. Demand fetches start at MaxDepth,
// so the links of an unreferenced JSON object are recorded but not followed.
// It wakes ingest through NOTIFY.
func (c *Catalog) Demand(ctx context.Context, root, subpath, site string) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO fetches (root_cid, subpath, lane, creator, depth)
		VALUES ($1, $2, 'demand', $3, $4)
		ON CONFLICT (root_cid, subpath) DO UPDATE SET lane = 'demand', next_try_at = now(), updated_at = now()
		WHERE fetches.status = 'pending' AND fetches.lane = 'backfill'
		  AND (fetches.lease_until IS NULL OR fetches.lease_until < now())`,
		root, subpath, site, MaxDepth); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, fetchChannel, LaneDemand); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Notifications calls fn with the lane named in each NOTIFY on the fetch
// channel until ctx ends or the dedicated connection fails.
func (c *Catalog) Notifications(ctx context.Context, fn func(lane string)) error {
	conn, err := pgx.ConnectConfig(ctx, c.pool.Config().ConnConfig)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+fetchChannel); err != nil {
		return err
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		fn(n.Payload)
	}
}
