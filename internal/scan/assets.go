package scan

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/algorand/go-algorand-sdk/v2/types"
	"github.com/jackc/pgx/v5"

	"seapin/internal/arcmeta"
	"seapin/internal/contract"
)

const maxNoteLinks = 32

// asset is the scanner's row for one asset.
type asset struct {
	found                              bool
	newestSeq                          int64
	destroyed                          bool
	url, name, unit, creator           string
	manager, reserve, freeze, clawback []byte
}

// LookupFunc returns an asset's creation parameters from algod; found is false
// for a destroyed asset. Live uses it when a reconfiguration arrives before
// the backfill has reached the asset's creation (the URL is only there).
type LookupFunc func(ctx context.Context, id uint64) (url, name, unit, creator string, found bool, err error)

// applyEvents records a block's configurations, in order, in their assets'
// rows and returns the references to publish. It costs two round trips per
// block however many events it has (2023 blocks carry ~300 asset
// configurations from inner transactions): one SELECT … FOR UPDATE of every
// asset involved, in asset order so live and backfill never deadlock, and one
// batch writing each changed asset's final state.
func (s *Scanner) applyEvents(ctx context.Context, tx pgx.Tx, evs []Event, src string) ([]contract.Ref, error) {
	ids := make([]int64, 0, len(evs))
	for _, ev := range evs {
		ids = append(ids, int64(ev.Asset))
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	assets, err := s.loadAssets(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	type change struct {
		refRound *int64
	}
	changed := map[uint64]*change{}
	var order []uint64
	var refs []contract.Ref
	for _, ev := range evs {
		a := assets[ev.Asset]
		r, dirty, err := s.apply(ctx, a, ev, src)
		if err != nil {
			return nil, fmt.Errorf("asset %d at round %d: %w", ev.Asset, ev.Round, err)
		}
		if !dirty {
			continue
		}
		a.found = true // later events of this block see the row as written
		c := changed[ev.Asset]
		if c == nil {
			c = &change{}
			changed[ev.Asset] = c
			order = append(order, ev.Asset)
		}
		for _, ref := range r {
			if rr := int64(ref.Round); c.refRound == nil || rr > *c.refRound {
				c.refRound = &rr
			}
		}
		refs = append(refs, r...)
	}
	if len(order) == 0 {
		return refs, nil
	}
	slices.Sort(order)
	var batch pgx.Batch
	for _, id := range order {
		s.queueSave(&batch, id, *assets[id], changed[id].refRound)
	}
	if err := tx.SendBatch(ctx, &batch).Close(); err != nil {
		return nil, err
	}
	return refs, nil
}

// apply records one configuration in its asset (locked by the caller's
// transaction) and returns the references to publish and whether the row
// changed. Configurations older than the newest one seen change nothing, so
// the backfill, walking down, resolves each asset once from its current
// addresses.
func (s *Scanner) apply(ctx context.Context, a *asset, ev Event, src string) ([]contract.Ref, bool, error) {
	var refs []contract.Ref
	switch ev.Kind {
	case Destroy:
		if a.found && a.newestSeq >= ev.Seq {
			return nil, false, nil
		}
		a.newestSeq, a.destroyed = ev.Seq, true

	case Reconfig:
		if a.found && a.newestSeq >= ev.Seq {
			return nil, false, nil
		}
		a.newestSeq, a.destroyed = ev.Seq, false
		a.setAddresses(ev.Params)
		if a.url == "" && src == contract.SrcLive && s.Lookup != nil {
			url, name, unit, creator, found, err := s.Lookup(ctx, ev.Asset)
			if err != nil {
				return nil, false, err
			}
			if found {
				a.setText(url, name, unit, creator)
			}
		}
		refs = append(refs, s.noteRefs(*a, ev, src)...)
		if a.url != "" {
			refs = append(refs, s.urlRef(*a, ev, ev.Round, src)...)
		}

	case Create:
		if !a.found {
			a.newestSeq = ev.Seq
			a.setAddresses(ev.Params)
			a.setText(ev.Params.URL, ev.Params.AssetName, ev.Params.UnitName, ev.Sender.String())
			refs = append(refs, s.noteRefs(*a, ev, src)...)
			refs = append(refs, s.urlRef(*a, ev, ev.Round, src)...)
			break
		}
		// A newer configuration was seen first: it holds the current addresses.
		if a.url != "" {
			return nil, false, nil
		}
		a.setText(ev.Params.URL, ev.Params.AssetName, ev.Params.UnitName, ev.Sender.String())
		if !a.destroyed {
			refs = append(refs, s.urlRef(*a, ev, uint64(a.newestSeq>>20), src)...)
		}
	}
	return refs, true, nil
}

// setText sets the asset's text fields. On chain they are arbitrary bytes, and
// Postgres text takes neither NUL nor invalid UTF-8: one such asset would
// otherwise fail its block's transaction on every retry and stall the cursor.
func (a *asset) setText(url, name, unit, creator string) {
	a.url, a.name, a.unit, a.creator = pgText(url), pgText(name), pgText(unit), pgText(creator)
}

func pgText(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "\uFFFD")
}

func (a *asset) setAddresses(p types.AssetParams) {
	a.manager, a.reserve, a.freeze, a.clawback = addr(p.Manager), addr(p.Reserve), addr(p.Freeze), addr(p.Clawback)
}

func addr(a types.Address) []byte {
	if a == (types.Address{}) {
		return nil
	}
	return a[:]
}

func (s *Scanner) urlRef(a asset, ev Event, round uint64, src string) []contract.Ref {
	r, ok := arcmeta.ResolveURL(a.url, ev.Asset, arcmeta.Addresses{
		Manager: a.manager, Reserve: a.reserve, Freeze: a.freeze, Clawback: a.clawback,
	})
	if !ok {
		return nil
	}
	return []contract.Ref{s.ref(a, ev, contract.PathURL, r, round, src)}
}

func (s *Scanner) noteRefs(a asset, ev Event, src string) []contract.Ref {
	var refs []contract.Ref
	for _, l := range arcmeta.NoteLinks(ev.Note, maxNoteLinks) {
		refs = append(refs, s.ref(a, ev, l.Path, l.Ref, ev.Round, src))
	}
	return refs
}

func (s *Scanner) ref(a asset, ev Event, path string, r arcmeta.Ref, round uint64, src string) contract.Ref {
	creator := a.creator
	if creator == "" {
		creator = ev.Sender.String() // a reconfiguration seen before the creation: its manager
	}
	return contract.NewRef(contract.Ref{
		Chain: s.Chain, Asset: ev.Asset, Path: path, CID: r.CID, Subpath: r.Subpath,
		Creator: creator, Src: src, Round: round, TxID: ev.TxID, BlockTime: ev.Time,
	})
}

// loadAssets locks and reads the given assets (sorted ids); an asset without
// a row comes back as a zero asset (found false).
func (s *Scanner) loadAssets(ctx context.Context, tx pgx.Tx, ids []int64) (map[uint64]*asset, error) {
	out := make(map[uint64]*asset, len(ids))
	for _, id := range ids {
		out[uint64(id)] = &asset{}
	}
	rows, err := tx.Query(ctx, `
		SELECT asset, newest_seq, destroyed, url, name, unit, creator, manager_addr, reserve_addr, freeze_addr, clawback_addr
		FROM scan_assets WHERE chain = $1 AND asset = ANY($2) ORDER BY asset FOR UPDATE`, s.Chain, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var url, name, unit, creator *string
		a := &asset{found: true}
		if err := rows.Scan(&id, &a.newestSeq, &a.destroyed, &url, &name, &unit, &creator,
			&a.manager, &a.reserve, &a.freeze, &a.clawback); err != nil {
			return nil, err
		}
		a.url, a.name, a.unit, a.creator = deref(url), deref(name), deref(unit), deref(creator)
		out[uint64(id)] = a
	}
	return out, rows.Err()
}

// queueSave queues the upsert of one asset's final state for this block;
// refRound is the newest round of the refs it produced (nil: none).
func (s *Scanner) queueSave(b *pgx.Batch, id uint64, a asset, refRound *int64) {
	b.Queue(`
		INSERT INTO scan_assets (chain, asset, newest_seq, destroyed, url, name, unit, creator,
		                         manager_addr, reserve_addr, freeze_addr, clawback_addr, ref_round)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), NULLIF($8, ''), $9, $10, $11, $12, $13)
		ON CONFLICT (chain, asset) DO UPDATE SET
		    newest_seq = EXCLUDED.newest_seq, destroyed = EXCLUDED.destroyed,
		    url = EXCLUDED.url, name = EXCLUDED.name, unit = EXCLUDED.unit, creator = EXCLUDED.creator,
		    manager_addr = EXCLUDED.manager_addr, reserve_addr = EXCLUDED.reserve_addr,
		    freeze_addr = EXCLUDED.freeze_addr, clawback_addr = EXCLUDED.clawback_addr,
		    ref_round = GREATEST(scan_assets.ref_round, EXCLUDED.ref_round),
		    meta_next_at = CASE WHEN EXCLUDED.ref_round IS NOT NULL THEN NULL ELSE scan_assets.meta_next_at END,
		    meta_tries = CASE WHEN EXCLUDED.ref_round IS NOT NULL THEN 0 ELSE scan_assets.meta_tries END`,
		s.Chain, int64(id), a.newestSeq, a.destroyed, a.url, a.name, a.unit, a.creator,
		a.manager, a.reserve, a.freeze, a.clawback, refRound)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
