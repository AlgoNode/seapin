package contract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const raw = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"

func validRef() Ref {
	return NewRef(Ref{
		Chain: "mainnet-v1.0", Asset: 1234567, Path: PathURL,
		CID: "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG", Subpath: "/meta.json",
		Creator: "CREATOR", Src: SrcLive, Round: 45000000, TxID: "TX",
		BlockTime: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
	})
}

func TestDecodeRefNormalizes(t *testing.T) {
	body, _ := json.Marshal(validRef())
	m, err := Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	r := m.(*Ref)
	if r.Key != "mainnet-v1.0/1234567/url" || !strings.HasPrefix(r.CID, "bafybei") || r.Subpath != "meta.json" {
		t.Errorf("got %+v", r)
	}
}

func TestRefValidation(t *testing.T) {
	bad := map[string]func(*Ref){
		"version":      func(r *Ref) { r.V = 2 },
		"chain slash":  func(r *Ref) { r.Chain = "a/b"; r.Key = KeyOf(r.Chain, r.Asset, r.Path) },
		"asset 0":      func(r *Ref) { r.Asset = 0; r.Key = KeyOf(r.Chain, 0, r.Path) },
		"path":         func(r *Ref) { r.Path = "url/image"; r.Key = KeyOf(r.Chain, r.Asset, r.Path) },
		"empty note":   func(r *Ref) { r.Path = "note/"; r.Key = KeyOf(r.Chain, r.Asset, r.Path) },
		"key mismatch": func(r *Ref) { r.Key = "x" },
		"src":          func(r *Ref) { r.Src = "other" },
		"round 0":      func(r *Ref) { r.Round = 0 },
		"cid":          func(r *Ref) { r.CID = "nope" },
		"subpath":      func(r *Ref) { r.Subpath = "a/../b" },
	}
	for name, mutate := range bad {
		r := validRef()
		mutate(&r)
		body, _ := json.Marshal(r)
		if _, err := Decode(body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r := validRef()
	r.Path = "note/media_url"
	r.Key = KeyOf(r.Chain, r.Asset, r.Path)
	if err := r.Validate(); err != nil {
		t.Errorf("note path: %v", err)
	}
}

func TestDecodeTakedown(t *testing.T) {
	body, _ := json.Marshal(NewTakedown(raw, "dmca"))
	m, err := Decode(body)
	if err != nil || m.(*Takedown).CID != raw {
		t.Fatalf("got %+v %v", m, err)
	}
	for _, b := range []string{`{"v":1,"type":"takedown","cid":"x","reason":"r"}`, `{"v":1,"type":"takedown","cid":"` + raw + `"}`, `{"v":1,"type":"nope"}`, `nope`} {
		if _, err := Decode([]byte(b)); err == nil {
			t.Errorf("%s: accepted", b)
		}
	}
}
