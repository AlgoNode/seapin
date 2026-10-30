package cidkey

import (
	"errors"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	mh "github.com/multiformats/go-multihash"
)

func TestNormalizeSpellingsOfOneObject(t *testing.T) {
	sum, _ := mh.Sum([]byte("hello"), mh.SHA2_256, -1)
	v0 := cid.NewCidV0(sum)
	v1 := cid.NewCidV1(cid.DagProtobuf, sum)
	want := v1.Encode(multibase.MustNewEncoder(multibase.Base32))

	for name, in := range map[string]string{
		"v0 base58":   v0.String(),
		"v1 base32":   want,
		"v1 base36":   v1.Encode(multibase.MustNewEncoder(multibase.Base36)),
		"v1 base58":   v1.Encode(multibase.MustNewEncoder(multibase.Base58BTC)),
		"v1 base32up": strings.ToUpper(want[:1]) + strings.ToUpper(want[1:]),
	} {
		got, err := Normalize(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
	}
	if !strings.HasPrefix(want, "bafybei") {
		t.Errorf("dag-pb CIDv1 should start with bafybei, got %s", want)
	}
}

func TestNormalizeRefuses(t *testing.T) {
	ident, _ := mh.Sum([]byte("<html>inline</html>"), mh.IDENTITY, -1)
	for name, in := range map[string]string{
		"empty":    "",
		"garbage":  "not-a-cid",
		"too long": "b" + strings.Repeat("a", 300),
		"identity": cid.NewCidV1(cid.Raw, ident).String(),
		"with dot": "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG.png",
	} {
		if _, err := Normalize(in); !errors.Is(err, ErrInvalidCID) {
			t.Errorf("%s: want ErrInvalidCID, got %v", name, err)
		}
	}
}

func TestObjectKey(t *testing.T) {
	got := ObjectKey("bafkreiabcdefgh")
	if want := "ef/gh/bafkreiabcdefgh"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestCleanSubpath(t *testing.T) {
	ok := map[string]string{
		"":                "",
		"/":               "",
		"1.png":           "1.png",
		"/a/b/c.json/":    "a/b/c.json",
		"name with space": "name with space",
		"ünïcode.png":     "ünïcode.png",
	}
	for in, want := range ok {
		got, err := CleanSubpath(in)
		if err != nil || got != want {
			t.Errorf("CleanSubpath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"a//b", "a/../b", "./a", "a/.", "a\\b", "a\x00b", "a\nb", strings.Repeat("a/", 40)} {
		if _, err := CleanSubpath(in); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanSubpath(%q): want ErrInvalidPath, got %v", in, err)
		}
	}
}

func TestVerifyRaw(t *testing.T) {
	data := []byte("raw block content")
	sum, _ := mh.Sum(data, mh.SHA2_256, -1)
	raw, _ := Normalize(cid.NewCidV1(cid.Raw, sum).String())

	if checked, err := VerifyRaw(raw, strings.NewReader(string(data))); !checked || err != nil {
		t.Errorf("matching content: checked=%v err=%v", checked, err)
	}
	if checked, err := VerifyRaw(raw, strings.NewReader("other")); !checked || err == nil {
		t.Errorf("different content: checked=%v err=%v, want a mismatch", checked, err)
	}
	dagpb, _ := Normalize(cid.NewCidV0(sum).String())
	if checked, err := VerifyRaw(dagpb, strings.NewReader("anything")); checked || err != nil {
		t.Errorf("dag-pb: checked=%v err=%v, want unchecked", checked, err)
	}
}
