// Package contract is the wire format between the scanner and seapin, version
// 1 (PLAN.md §1 Message contract). References and takedowns go to the fanout
// exchange "seapin", one queue per region. Ingest reports what happened to each
// object on the topic exchange "seapin.events" (routing key "<region>.<status>").
package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"seapin/internal/cidkey"
)

const (
	Version = 1

	ExchangeRefs   = "seapin"
	ExchangeEvents = "seapin.events"

	TypeRef      = "ref"
	TypeTakedown = "takedown"
	TypeObject   = "object"

	SrcLive     = "live"
	SrcBackfill = "backfill"

	// PathURL is the ASA URL field; PathNotePrefix starts the JSON pointer of a
	// link inside an ARC-69 note ("note/media_url").
	PathURL        = "url"
	PathNotePrefix = "note/"
)

// Ref is one chain-level reference: an asset's URL field or a link in its
// ARC-69 note, resolved to a CID (and a path inside it). Several refs may share
// a CID; the latest round per key wins.
type Ref struct {
	V         int       `json:"v"`
	Type      string    `json:"type"`
	Key       string    `json:"key"`
	Chain     string    `json:"chain"`
	Asset     uint64    `json:"asset"`
	Path      string    `json:"path"`
	CID       string    `json:"cid"`
	Subpath   string    `json:"subpath,omitempty"`
	MimeHint  string    `json:"mime_hint,omitempty"`
	Creator   string    `json:"creator"`
	Src       string    `json:"src"`
	Round     uint64    `json:"round"`
	TxID      string    `json:"txid,omitempty"`
	BlockTime time.Time `json:"block_time"`
}

// Takedown is an operator block: the CID is answered with 451 in every region.
type Takedown struct {
	V      int    `json:"v"`
	Type   string `json:"type"`
	CID    string `json:"cid"`
	Reason string `json:"reason"`
}

// Event reports an ingest outcome for one fetch (root CID + path) and the leaf
// object it produced.
type Event struct {
	V       int       `json:"v"`
	Type    string    `json:"type"`
	Region  string    `json:"region"`
	Root    string    `json:"root"`
	Subpath string    `json:"subpath,omitempty"`
	CID     string    `json:"cid,omitempty"` // leaf; empty when the fetch failed
	Status  string    `json:"status"`        // cached | blocked | failed
	Mime    string    `json:"mime,omitempty"`
	Size    int64     `json:"size,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Source  string    `json:"source,omitempty"`
	At      time.Time `json:"at"`
}

// KeyOf is the catalog key of a reference.
func KeyOf(chain string, asset uint64, path string) string {
	return fmt.Sprintf("%s/%d/%s", chain, asset, path)
}

// NewRef fills the version, type and key of a reference.
func NewRef(r Ref) Ref {
	r.V, r.Type = Version, TypeRef
	r.Key = KeyOf(r.Chain, r.Asset, r.Path)
	return r
}

func NewTakedown(cid, reason string) Takedown {
	return Takedown{V: Version, Type: TypeTakedown, CID: cid, Reason: reason}
}

// Validate checks a reference and normalizes its CID and sub-path in place.
func (r *Ref) Validate() error {
	switch {
	case r.V != Version || r.Type != TypeRef:
		return fmt.Errorf("ref: version %d type %q", r.V, r.Type)
	case r.Chain == "" || strings.ContainsAny(r.Chain, "/ "):
		return fmt.Errorf("ref: bad chain %q", r.Chain)
	case r.Asset == 0:
		return errors.New("ref: asset 0")
	case !validPath(r.Path):
		return fmt.Errorf("ref: bad path %q", r.Path)
	case r.Key != KeyOf(r.Chain, r.Asset, r.Path):
		return fmt.Errorf("ref: key %q does not match chain/asset/path", r.Key)
	case r.Src != SrcLive && r.Src != SrcBackfill:
		return fmt.Errorf("ref: bad src %q", r.Src)
	case r.Round == 0:
		return errors.New("ref: round 0")
	}
	c, err := cidkey.Normalize(r.CID)
	if err != nil {
		return fmt.Errorf("ref: %w", err)
	}
	sub, err := cidkey.CleanSubpath(r.Subpath)
	if err != nil {
		return fmt.Errorf("ref: %w", err)
	}
	r.CID, r.Subpath = c, sub
	return nil
}

func validPath(p string) bool {
	if p == PathURL {
		return true
	}
	rest, ok := strings.CutPrefix(p, PathNotePrefix)
	if !ok || rest == "" {
		return false
	}
	return !slices.Contains(strings.Split(rest, "/"), "")
}

// Decode parses a message from the "seapin" exchange into a validated *Ref or
// *Takedown.
func Decode(body []byte) (any, error) {
	var head struct {
		V    int    `json:"v"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if head.V != Version {
		return nil, fmt.Errorf("decode: unsupported version %d", head.V)
	}
	switch head.Type {
	case TypeRef:
		var r Ref
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("decode ref: %w", err)
		}
		if err := r.Validate(); err != nil {
			return nil, err
		}
		return &r, nil
	case TypeTakedown:
		var t Takedown
		if err := json.Unmarshal(body, &t); err != nil {
			return nil, fmt.Errorf("decode takedown: %w", err)
		}
		c, err := cidkey.Normalize(t.CID)
		if err != nil {
			return nil, fmt.Errorf("takedown: %w", err)
		}
		if t.Reason == "" {
			return nil, errors.New("takedown: empty reason")
		}
		t.CID = c
		return &t, nil
	}
	return nil, fmt.Errorf("decode: unknown type %q", head.Type)
}
