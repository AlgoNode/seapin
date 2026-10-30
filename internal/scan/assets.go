package scan

import (
	"context"
	"errors"

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

// apply records one configuration in the asset's row (locked by the caller's
// transaction) and returns the references to publish. Configurations older
// than the newest one seen change nothing, so the backfill, walking down,
// resolves each asset once from its current addresses.
func (s *Scanner) apply(ctx context.Context, tx pgx.Tx, ev Event, src string) ([]contract.Ref, error) {
	a, err := s.loadAsset(ctx, tx, ev.Asset)
	if err != nil {
		return nil, err
	}
	var refs []contract.Ref
	switch ev.Kind {
	case Destroy:
		if a.found && a.newestSeq >= ev.Seq {
			return nil, nil
		}
		a.newestSeq, a.destroyed = ev.Seq, true

	case Reconfig:
		if a.found && a.newestSeq >= ev.Seq {
			return nil, nil
		}
		a.newestSeq, a.destroyed = ev.Seq, false
		a.setAddresses(ev.Params)
		if a.url == "" && src == contract.SrcLive && s.Lookup != nil {
			url, name, unit, creator, found, err := s.Lookup(ctx, ev.Asset)
			if err != nil {
				return nil, err
			}
			if found {
				a.url, a.name, a.unit, a.creator = url, name, unit, creator
			}
		}
		refs = append(refs, s.noteRefs(a, ev, src)...)
		if a.url != "" {
			refs = append(refs, s.urlRef(a, ev, ev.Round, src)...)
		}

	case Create:
		if !a.found {
			a.newestSeq = ev.Seq
			a.setAddresses(ev.Params)
			a.url, a.name, a.unit, a.creator = ev.Params.URL, ev.Params.AssetName, ev.Params.UnitName, ev.Sender.String()
			refs = append(refs, s.noteRefs(a, ev, src)...)
			refs = append(refs, s.urlRef(a, ev, ev.Round, src)...)
			break
		}
		// A newer configuration was seen first: it holds the current addresses.
		if a.url != "" {
			return nil, nil
		}
		a.url, a.name, a.unit, a.creator = ev.Params.URL, ev.Params.AssetName, ev.Params.UnitName, ev.Sender.String()
		if !a.destroyed {
			refs = append(refs, s.urlRef(a, ev, uint64(a.newestSeq>>20), src)...)
		}
	}
	return refs, s.saveAsset(ctx, tx, ev.Asset, a, refs)
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

func (s *Scanner) loadAsset(ctx context.Context, tx pgx.Tx, id uint64) (asset, error) {
	var a asset
	var url, name, unit, creator *string
	err := tx.QueryRow(ctx, `
		SELECT newest_seq, destroyed, url, name, unit, creator, manager_addr, reserve_addr, freeze_addr, clawback_addr
		FROM scan_assets WHERE chain = $1 AND asset = $2 FOR UPDATE`, s.Chain, int64(id),
	).Scan(&a.newestSeq, &a.destroyed, &url, &name, &unit, &creator, &a.manager, &a.reserve, &a.freeze, &a.clawback)
	if errors.Is(err, pgx.ErrNoRows) {
		return asset{}, nil
	}
	if err != nil {
		return asset{}, err
	}
	a.found = true
	a.url, a.name, a.unit, a.creator = deref(url), deref(name), deref(unit), deref(creator)
	return a, nil
}

func (s *Scanner) saveAsset(ctx context.Context, tx pgx.Tx, id uint64, a asset, refs []contract.Ref) error {
	var refRound *int64
	for _, r := range refs {
		rr := int64(r.Round)
		if refRound == nil || rr > *refRound {
			refRound = &rr
		}
	}
	_, err := tx.Exec(ctx, `
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
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
