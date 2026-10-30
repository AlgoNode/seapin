// Package pgmigrate applies embedded SQL migrations in file-name order, each
// in its own transaction, and records them in a table. An advisory lock keeps
// two processes from applying them at the same time.
package pgmigrate

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Run applies the files matching pattern in fsys that table does not list yet.
// table is a fixed name chosen by the caller, never user input.
func Run(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, pattern, table string, lock int64) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lock); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lock)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(fsys, pattern)
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM `+table+` WHERE version = $1)`, name,
		).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+table+` (version) VALUES ($1)`, name); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
