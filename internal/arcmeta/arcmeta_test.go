package arcmeta

import (
	"strings"
	"testing"

	"github.com/algorand/go-algorand-sdk/v2/types"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"seapin/internal/cidkey"
)

const (
	v0  = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"
	v1  = "bafybeie5gq4jxvzmsym6hjlwxej4rwdoxt7wadqvmmwbqi7r27fclha2va"
	raw = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
)

func norm(t *testing.T, s string) string {
	t.Helper()
	n, err := cidkey.Normalize(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestParseIPFS(t *testing.T) {
	cases := map[string]Ref{
		"ipfs://" + v0:                                      {norm(t, v0), ""},
		"ipfs://" + v0 + "#arc3":                            {norm(t, v0), ""},
		"IPFS://" + raw:                                     {raw, ""},
		"ipfs://ipfs/" + raw:                                {raw, ""},
		"ipfs://" + v1 + "/metadata.json":                   {v1, "metadata.json"},
		"ipfs://" + v1 + "/My%20Image.png?x=1":              {v1, "My Image.png"},
		"  ipfs://" + raw + "  ":                            {raw, ""},
		"https://gateway.pinata.cloud/ipfs/" + v0:           {norm(t, v0), ""},
		"https://ipfs.io/ipfs/" + v1 + "/a/b.png":           {v1, "a/b.png"},
		"https://" + v1 + ".ipfs.dweb.link/1.json":          {v1, "1.json"},
		"http://" + strings.ToUpper(raw) + ".ipfs.w3s.link": {raw, ""},
	}
	for in, want := range cases {
		got, ok := ParseIPFS(in)
		if !ok || got != want {
			t.Errorf("ParseIPFS(%q) = %+v, %v; want %+v", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"", v0, "https://example.com/image.png", "ar://abc", "ipfs://", "ipfs://not-a-cid",
		"ipfs://" + raw + "/../x", "javascript:alert(1)", "data:image/png;base64,AAAA",
		"https://example.com/ipfs/not-a-cid", "ipfs://" + strings.Repeat("a", 3000),
	} {
		if got, ok := ParseIPFS(in); ok {
			t.Errorf("ParseIPFS(%q) = %+v, want no reference", in, got)
		}
	}
}

func TestResolveURLARC3ID(t *testing.T) {
	got, ok := ResolveURL("ipfs://"+v1+"/{id}.json#arc3", 1234567, Addresses{})
	if !ok || got != (Ref{v1, "1234567.json"}) {
		t.Errorf("got %+v %v", got, ok)
	}
}

func addr(t *testing.T, a string) []byte {
	t.Helper()
	d, err := types.DecodeAddress(a)
	if err != nil {
		t.Fatal(err)
	}
	return d[:]
}

// The example from the ARC-19 specification.
func TestResolveURLARC19SpecExample(t *testing.T) {
	reserve := addr(t, "EEQYWGGBHRDAMTEVDPVOSDVX3HJQIG6K6IVNR3RXHYOHV64ZWAEISS4CTI")
	got, ok := ResolveURL("template-ipfs://{ipfscid:0:dag-pb:reserve:sha2-256}", 1, Addresses{Reserve: reserve})
	if want := norm(t, "QmQZyq4b89RfaUw8GESPd2re4hJqB8bnm4kVHNtyQrHnnK"); !ok || got.CID != want {
		t.Errorf("got %+v %v, want %s", got, ok, want)
	}
}

func TestResolveURLARC19NamedField(t *testing.T) {
	digest := func(s string) []byte {
		sum, _ := mh.Sum([]byte(s), mh.SHA2_256, -1)
		dec, _ := mh.Decode(sum)
		return dec.Digest
	}
	a := Addresses{Reserve: digest("reserve"), Manager: digest("manager")}
	want := func(d []byte, codec uint64) string {
		sum, _ := mh.Encode(d, mh.SHA2_256)
		return norm(t, cid.NewCidV1(codec, sum).String())
	}

	got, ok := ResolveURL("template-ipfs://{ipfscid:1:raw:reserve:sha2-256}/meta.json#arc3", 9, a)
	if !ok || got != (Ref{want(a.Reserve, cid.Raw), "meta.json"}) {
		t.Errorf("reserve: got %+v %v", got, ok)
	}
	got, ok = ResolveURL("template-ipfs://{ipfscid:1:dag-pb:manager:sha2-256}", 9, a)
	if !ok || got.CID != want(a.Manager, cid.DagProtobuf) {
		t.Errorf("manager: got %+v %v (today's scanner would have used the reserve)", got, ok)
	}
	for name, u := range map[string]string{
		"unset freeze":  "template-ipfs://{ipfscid:1:raw:freeze:sha2-256}",
		"unknown field": "template-ipfs://{ipfscid:1:raw:creator:sha2-256}",
		"unknown hash":  "template-ipfs://{ipfscid:1:raw:reserve:sha3-256}",
		"unknown codec": "template-ipfs://{ipfscid:1:git-raw:reserve:sha2-256}",
		"v0 not dag-pb": "template-ipfs://{ipfscid:0:raw:reserve:sha2-256}",
		"bad version":   "template-ipfs://{ipfscid:2:raw:reserve:sha2-256}",
	} {
		if got, ok := ResolveURL(u, 9, a); ok {
			t.Errorf("%s: got %+v, want no reference", name, got)
		}
	}
}

func TestLinks(t *testing.T) {
	doc := `{
	  "name": "x", "external_url": "https://example.com",
	  "properties": {"file_url": "ipfs://` + raw + `", "files": [{"uri": "ipfs://` + v1 + `/2.png"}], "a/b~c": "ipfs://` + raw + `"},
	  "image": "ipfs://` + v0 + `",
	  "animation_url": "https://ipfs.io/ipfs/` + v1 + `/anim.mp4",
	  "description": "see ipfs://not-a-cid"
	}`
	links, err := Links([]byte(doc), 32)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range links {
		got = append(got, l.Path+"="+l.CID[len(l.CID)-4:]+"/"+l.Subpath)
	}
	want := []string{
		"image=" + norm(t, v0)[len(norm(t, v0))-4:] + "/",
		"animation_url=" + v1[len(v1)-4:] + "/anim.mp4",
		"properties/file_url=" + raw[len(raw)-4:] + "/",
		"properties/a~1b~0c=" + raw[len(raw)-4:] + "/",
		"properties/files/0/uri=" + v1[len(v1)-4:] + "/2.png",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("links:\n got  %v\n want %v", got, want)
	}

	if links, _ := Links([]byte(doc), 2); len(links) != 2 || links[0].Path != "image" || links[1].Path != "animation_url" {
		t.Errorf("max 2 should keep the well-known first: %+v", links)
	}
	if _, err := Links([]byte(`{"broken"`), 32); err == nil {
		t.Error("broken JSON accepted")
	}
	deep := strings.Repeat(`{"a":`, 40) + `"ipfs://` + raw + `"` + strings.Repeat("}", 40)
	if links, _ := Links([]byte(deep), 32); len(links) != 0 {
		t.Errorf("link deeper than the walk limit was followed: %+v", links)
	}
}

func TestNoteLinks(t *testing.T) {
	note := []byte(`{"standard":"arc69","description":"d","media_url":"ipfs://` + raw + `#i","mime_type":"image/png"}`)
	links := NoteLinks(note, 32)
	if len(links) != 1 || links[0].Path != "note/media_url" || links[0].CID != raw {
		t.Errorf("got %+v", links)
	}
	// A NUL in a key (paths are Postgres text) is dropped.
	if links := NoteLinks([]byte(`{"standard":"arc69","media\u0000_url":"ipfs://`+raw+`"}`), 32); len(links) != 1 || links[0].Path != "note/media_url" {
		t.Errorf("NUL key: got %+v", links)
	}
	for _, n := range []string{`{"media_url":"ipfs://` + raw + `"}`, `hello`, ``, `{"standard":"arc3"}`} {
		if links := NoteLinks([]byte(n), 32); len(links) != 0 {
			t.Errorf("%q: got %+v", n, links)
		}
	}
}
