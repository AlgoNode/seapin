package catalog

import (
	"context"
	"time"

	"seapin/internal/contract"
)

// RefsBefore returns up to limit references last written before the given
// time, in key order after afterKey, as backfill refs ready to republish. The
// catalog does not keep a ref's src or mime hint; both are hints only.
func (c *Catalog) RefsBefore(ctx context.Context, before time.Time, afterKey string, limit int) ([]contract.Ref, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT key, chain, asset, path, root_cid, subpath, creator, round, txid, block_time
		FROM refs WHERE key > $1 AND updated_at < $2 ORDER BY key LIMIT $3`, afterKey, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contract.Ref
	for rows.Next() {
		var (
			r             contract.Ref
			key           string
			asset, round  int64
			creator, txid *string
			blockTime     *time.Time
		)
		if err := rows.Scan(&key, &r.Chain, &asset, &r.Path, &r.CID, &r.Subpath, &creator, &round, &txid, &blockTime); err != nil {
			return nil, err
		}
		r.Asset, r.Round, r.Creator, r.TxID, r.Src = uint64(asset), uint64(round), deref(creator), deref(txid), contract.SrcBackfill
		if blockTime != nil {
			r.BlockTime = *blockTime
		}
		r = contract.NewRef(r)
		r.Key = key // as stored, so a resumed run continues from exactly this row
		out = append(out, r)
	}
	return out, rows.Err()
}
