// Package arcmeta finds IPFS references in Algorand asset metadata: the ASA
// URL field (IPFS URLs, ARC-3 {id} templates, ARC-19 template-ipfs), ARC-69
// notes, and IPFS links inside metadata JSON. The scanner uses it for
// chain-level references, ingest for links inside stored JSON.
package arcmeta

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"seapin/internal/cidkey"
)

// Ref is an IPFS reference: a normalized root CID and a path inside it.
type Ref struct {
	CID     string
	Subpath string
}

const maxURILen = 2048

// ParseIPFS recognizes the explicit IPFS forms only:
//
//	ipfs://<cid>[/<path>]               (also ipfs://ipfs/<cid>…)
//	http(s)://<host>/ipfs/<cid>[/<path>]
//	http(s)://<cid>.ipfs.<host>[/<path>]
//
// Query strings and fragments (#arc3) are dropped. Bare CIDs and other URLs are
// not references (decision L), nor is anything whose CID or path is invalid.
func ParseIPFS(s string) (Ref, bool) {
	s = strings.TrimSpace(s)
	if len(s) > maxURILen {
		return Ref{}, false
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	var rest string
	if p, ok := cutPrefixFold(s, "ipfs://"); ok {
		rest, _ = cutPrefixFold(p, "ipfs/")
	} else {
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Ref{}, false
		}
		host := strings.ToLower(u.Hostname())
		if i := strings.Index(host, ".ipfs."); i > 0 {
			rest = host[:i] + u.Path
		} else if p, ok := strings.CutPrefix(u.Path, "/ipfs/"); ok {
			rest = p
		} else {
			return Ref{}, false
		}
	}
	return newRef(rest)
}

func newRef(cidAndPath string) (Ref, bool) {
	c, sub, _ := strings.Cut(cidAndPath, "/")
	norm, err := cidkey.Normalize(c)
	if err != nil {
		return Ref{}, false
	}
	if un, err := url.PathUnescape(sub); err == nil {
		sub = un
	}
	sub, err = cidkey.CleanSubpath(sub)
	if err != nil {
		return Ref{}, false
	}
	return Ref{CID: norm, Subpath: sub}, true
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// Addresses are an asset's role addresses as 32-byte public keys; nil when unset.
type Addresses struct {
	Manager, Reserve, Freeze, Clawback []byte
}

var arc19 = regexp.MustCompile(`^template-ipfs://\{ipfscid:([01]):([a-z0-9-]+):([a-z]+):([a-z0-9-]+)\}(.*)$`)

var codecs = map[string]uint64{
	"raw":      cid.Raw,
	"dag-pb":   cid.DagProtobuf,
	"dag-cbor": cid.DagCBOR,
	"dag-json": cid.DagJSON,
}

// ResolveURL turns an ASA's URL field into an IPFS reference. It substitutes
// ARC-3's {id} with the asset id and expands ARC-19 templates, whose CID digest
// is the address in the role field the template names (reserve, manager,
// freeze or clawback), before ParseIPFS.
func ResolveURL(raw string, asset uint64, addrs Addresses) (Ref, bool) {
	raw = strings.ReplaceAll(strings.TrimSpace(raw), "{id}", strconv.FormatUint(asset, 10))
	if m := arc19.FindStringSubmatch(raw); m != nil {
		return resolveARC19(m[1], m[2], m[3], m[4], m[5], addrs)
	}
	return ParseIPFS(raw)
}

func resolveARC19(version, codecName, field, hashName, rest string, a Addresses) (Ref, bool) {
	var digest []byte
	switch field {
	case "reserve":
		digest = a.Reserve
	case "manager":
		digest = a.Manager
	case "freeze":
		digest = a.Freeze
	case "clawback":
		digest = a.Clawback
	default:
		return Ref{}, false
	}
	codec, ok := codecs[codecName]
	if !ok || hashName != "sha2-256" || len(digest) != 32 {
		return Ref{}, false
	}
	sum, err := mh.Encode(digest, mh.SHA2_256)
	if err != nil {
		return Ref{}, false
	}
	var c cid.Cid
	if version == "0" {
		if codec != cid.DagProtobuf {
			return Ref{}, false
		}
		c = cid.NewCidV0(sum)
	} else {
		c = cid.NewCidV1(codec, sum)
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	return newRef(c.String() + "/" + strings.TrimPrefix(rest, "/"))
}

// Link is an IPFS reference found inside a JSON document. Path is its JSON
// pointer without the leading slash (RFC 6901 escaping), e.g. "properties/file_url".
type Link struct {
	Path string
	Ref
}

const (
	maxJSONDepth  = 32
	maxCandidates = 1024
)

// wellKnown links are kept first when a document has more than max links.
var wellKnown = map[string]int{
	"image":               0,
	"animation_url":       1,
	"properties/file_url": 2,
	"media_url":           3,
}

// Links returns up to max explicit IPFS links in a JSON document: the
// well-known ARC fields first, then the rest in path order. The same bytes
// always give the same links.
func Links(doc []byte, max int) ([]Link, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var out []Link
	walk(v, "", 0, &out)
	sort.SliceStable(out, func(i, j int) bool {
		ri, iok := wellKnown[out[i].Path]
		rj, jok := wellKnown[out[j].Path]
		switch {
		case iok && jok:
			return ri < rj
		case iok != jok:
			return iok
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

func walk(v any, path string, depth int, out *[]Link) {
	if len(*out) >= maxCandidates || depth > maxJSONDepth {
		return
	}
	switch t := v.(type) {
	case string:
		if path == "" {
			return
		}
		if r, ok := ParseIPFS(t); ok {
			*out = append(*out, Link{Path: path, Ref: r})
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walk(t[k], join(path, escape(k)), depth+1, out)
		}
	case []any:
		for i, e := range t {
			walk(e, join(path, strconv.Itoa(i)), depth+1, out)
		}
	}
}

func join(path, seg string) string {
	if path == "" {
		return seg
	}
	return path + "/" + seg
}

func escape(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}

// NoteLinks returns the IPFS links in an ARC-69 note (a JSON object with
// "standard": "arc69"), with paths prefixed by "note/". Other notes give none.
func NoteLinks(note []byte, max int) []Link {
	note = bytes.TrimSpace(note)
	if len(note) == 0 || note[0] != '{' {
		return nil
	}
	var head struct {
		Standard string `json:"standard"`
	}
	if json.Unmarshal(note, &head) != nil || !strings.EqualFold(head.Standard, "arc69") {
		return nil
	}
	links, err := Links(note, max)
	if err != nil {
		return nil
	}
	for i := range links {
		links[i].Path = "note/" + links[i].Path
	}
	return links
}
