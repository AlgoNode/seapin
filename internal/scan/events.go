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
	for _, st := range b.Payset {
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
