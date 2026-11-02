package scan

import (
	"context"
	"time"

	"seapin/internal/contract"
)

// ReresolveStats counts one Reresolve pass.
type ReresolveStats struct {
	Scanned, Resolved, Published int
}

// Reresolve applies the current URL parser to assets that never produced a
// reference: live assets with a URL and no ref_round. A parser fix (scheme-less
// gateway URLs, 2026-11-02) then reaches existing assets without walking the
// chain again. Each resolved URL is published as the asset's "url" ref, dated
// by the block of its newest configuration, and ref_round is set, so a second
// pass skips it. dryRun only counts.
func (s *Scanner) Reresolve(ctx context.Context, dryRun bool) (ReresolveStats, error) {
	var st ReresolveStats
	var last int64 = -1
	for {
		rows, err := s.DB.Query(ctx, `
			SELECT asset, newest_seq, url, coalesce(name, ''), coalesce(unit, ''), coalesce(creator, ''),
			       manager_addr, reserve_addr, freeze_addr, clawback_addr
			FROM scan_assets
			WHERE chain = $1 AND asset > $2 AND NOT destroyed AND url IS NOT NULL AND ref_round IS NULL
			ORDER BY asset LIMIT 1000`, s.Chain, last)
		if err != nil {
			return st, err
		}
		type cand struct {
			id uint64
			a  asset
		}
		var page []cand
		for rows.Next() {
			var id int64
			a := asset{found: true}
			if err := rows.Scan(&id, &a.newestSeq, &a.url, &a.name, &a.unit, &a.creator,
				&a.manager, &a.reserve, &a.freeze, &a.clawback); err != nil {
				rows.Close()
				return st, err
			}
			page = append(page, cand{uint64(id), a})
			last = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return st, err
		}
		if len(page) == 0 {
			return st, nil
		}
		for _, c := range page {
			st.Scanned++
			round := uint64(c.a.newestSeq >> 20)
			ev := Event{Asset: c.id, Round: round, Seq: c.a.newestSeq}
			refs := s.urlRef(c.a, ev, round, contract.SrcBackfill)
			if len(refs) == 0 {
				continue
			}
			st.Resolved++
			if dryRun {
				continue
			}
			hdr, err := s.Blocks.Block(round).HeaderOnly(true).Do(ctx)
			if err != nil {
				return st, err
			}
			for i := range refs {
				refs[i].BlockTime = time.Unix(hdr.TimeStamp, 0).UTC()
			}
			if err := s.Backfill.Publish(ctx, refs); err != nil {
				return st, err
			}
			if _, err := s.DB.Exec(ctx, `UPDATE scan_assets SET ref_round = $3, meta_next_at = NULL, meta_tries = 0
				WHERE chain = $1 AND asset = $2`, s.Chain, int64(c.id), int64(round)); err != nil {
				return st, err
			}
			st.Published += len(refs)
			s.refs.WithLabelValues(contract.SrcBackfill).Add(float64(len(refs)))
		}
		s.Log.Info("reresolve", "scanned", st.Scanned, "resolved", st.Resolved, "published", st.Published, "last_asset", last)
	}
}
