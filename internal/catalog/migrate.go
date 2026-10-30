package catalog

import (
	"context"
	"embed"

	"seapin/internal/pgmigrate"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies the catalog's embedded migrations.
func (c *Catalog) Migrate(ctx context.Context) error {
	return pgmigrate.Run(ctx, c.pool, migrations, "migrations/*.sql", "schema_migrations", 0x5ea_0001)
}
