package catalog

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// openTest returns a catalog in a throwaway schema of the database named by
// SEAPIN_TEST_PG_DSN (e.g. the dev stack's Postgres); without it the test is skipped.
func openTest(t *testing.T) *Catalog {
	t.Helper()
	dsn := os.Getenv("SEAPIN_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SEAPIN_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		conn.Close(context.Background())
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	c, err := Open(ctx, dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestCatalog(t *testing.T) {
	c := openTest(t)
	ctx := context.Background()
	for range 2 { // migrations are idempotent
		if err := c.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if _, found, err := c.Lookup(ctx, "bafkunknown"); err != nil || found {
		t.Fatalf("unknown CID: found=%v err=%v", found, err)
	}
	if err := c.MarkBlocked(ctx, "bafkhtml", "text/html", 120, "not_allowed:text/html"); err != nil {
		t.Fatal(err)
	}
	o, found, err := c.Lookup(ctx, "bafkhtml")
	if err != nil || !found || o.Status != StatusBlocked || o.Reason != "not_allowed:text/html" || o.Size != 120 {
		t.Fatalf("blocked row: %+v found=%v err=%v", o, found, err)
	}

	if err := c.MarkCached(ctx, "bafkpng", "image/png", 608, "seed"); err != nil {
		t.Fatal(err)
	}
	// A later refusal must not downgrade a cached object (append-only).
	if err := c.MarkBlocked(ctx, "bafkpng", "text/html", 1, "not_allowed:text/html"); err != nil {
		t.Fatal(err)
	}
	if o, _, _ := c.Lookup(ctx, "bafkpng"); o.Status != StatusCached || o.Mime != "image/png" {
		t.Fatalf("cached row changed: %+v", o)
	}

	if _, err := c.pool.Exec(ctx, `INSERT INTO aliases VALUES ('bafyroot', 'images/1.png', 'bafkpng')`); err != nil {
		t.Fatal(err)
	}
	if leaf, found, err := c.ResolveAlias(ctx, "bafyroot", "images/1.png"); err != nil || !found || leaf != "bafkpng" {
		t.Fatalf("alias: %q found=%v err=%v", leaf, found, err)
	}
	if _, found, _ := c.ResolveAlias(ctx, "bafyroot", "images/2.png"); found {
		t.Fatal("unknown alias found")
	}

	if _, err := c.pool.Exec(ctx, `INSERT INTO takedowns (cid, reason) VALUES ('bafkpng', 'dmca')`); err != nil {
		t.Fatal(err)
	}
	if td, err := c.Takedowns(ctx); err != nil || td["bafkpng"] != "dmca" || len(td) != 1 {
		t.Fatalf("takedowns: %v err=%v", td, err)
	}
}
