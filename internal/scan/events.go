// Package scan is seapin-scan, the rewrite of algoipfssvc (PLAN §1 Scanner).
// It reads Algorand blocks with two cursors (live from the tip at bootstrap
// upwards, backfill from there down to genesis), turns asset configurations
// into chain-level IPFS references and publishes them to the "seapin"
// exchange. It never fetches from IPFS; seapin does, and follows JSON links.
package scan

import (
	"time"

	"github.com/algorand/go-algorand-sdk/v2/crypto"
	"github.com/algorand/go-algorand-sdk/v2/types"
)

type Kind int

const (
	Create Kind = iota + 1
	Reconfig
	Destroy
)

// Event is one asset configuration transaction, top-level or inner.
type Event struct {
	Asset  uint64
	Kind   Kind
	Round  uint64
	Seq    int64 // (round << 20) | position in the block: orders configurations of one asset
	Time   time.Time
	TxID   string // top-level transactions only
	Sender types.Address
	Params types.AssetParams // create: all fields; reconfig: the new role addresses
	Note   []byte
}

// Events returns a block's asset configurations in block order, inner
// transactions included: apps mint assets through inner acfg transactions,
// and the id of a created asset is only in the ApplyData.
func Events(b types.Block) []Event {
	var out []Event
	t := time.Unix(b.TimeStamp, 0).UTC()
	pos := 0
	// Blocks before the protocol upgrade at round ~16.69 M do not record a
	// created asset's id in ApplyData. It is then the transaction counter at
	// that transaction: TxnCounter counts up to the block's last transaction,
	// one per top-level transaction (those blocks have no inner ones). Without
	// this, every asset created then and never reconfigured was missed
	// (M6 coverage check, 2026-11-02: 27.8 k assets).
	derive := b.TxnCounter >= uint64(len(b.Payset)) && !hasInner(b)
	for i, st := range b.Payset {
		if derive && st.Txn.Type == types.AssetConfigTx && st.Txn.ConfigAsset == 0 && st.ApplyData.ConfigAsset == 0 {
			st.ApplyData.ConfigAsset = types.AssetIndex(b.TxnCounter - uint64(len(b.Payset)) + uint64(i) + 1)
		}
		txid := ""
		if st.Txn.Type == types.AssetConfigTx {
			txn := st.Txn
			if st.HasGenesisID {
				txn.GenesisID = b.GenesisID
			}
			if st.HasGenesisHash {
				txn.GenesisHash = b.GenesisHash
			}
			txid = crypto.GetTxID(txn)
		}
		collect(&out, st.SignedTxnWithAD, uint64(b.Round), t, txid, &pos)
	}
	return out
}

func collect(out *[]Event, s types.SignedTxnWithAD, round uint64, t time.Time, txid string, pos *int) {
	if s.Txn.Type == types.AssetConfigTx {
		ev := Event{
			Asset: uint64(s.Txn.ConfigAsset), Round: round, Seq: int64(round)<<20 | int64(*pos&0xfffff),
			Time: t, TxID: txid, Sender: s.Txn.Sender, Params: s.Txn.AssetParams, Note: s.Txn.Note,
		}
		switch {
		case ev.Asset == 0:
			ev.Asset, ev.Kind = uint64(s.ApplyData.ConfigAsset), Create
		case s.Txn.AssetParams == (types.AssetParams{}):
			ev.Kind = Destroy
		default:
			ev.Kind = Reconfig
		}
		if ev.Asset != 0 {
			*out = append(*out, ev)
		}
	}
	*pos++
	for _, in := range s.ApplyData.EvalDelta.InnerTxns {
		collect(out, in, round, t, "", pos)
	}
}

func hasInner(b types.Block) bool {
	for _, st := range b.Payset {
		if len(st.ApplyData.EvalDelta.InnerTxns) > 0 {
			return true
		}
	}
	return false
}
