// Package cidkey turns CID strings from requests and from the chain into the
// one form seapin stores and serves: CIDv1 in base32, with a sharded S3 key.
package cidkey

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	mh "github.com/multiformats/go-multihash"
)

var (
	ErrInvalidCID  = errors.New("invalid CID")
	ErrInvalidPath = errors.New("invalid path")
)

const (
	maxCIDLen     = 256
	maxSubpathLen = 1024
	maxSegments   = 32
)

// Normalize parses s and returns it as CIDv1 in base32. A CIDv0 (Qm…) becomes
// the CIDv1 dag-pb CID with the same multihash, so both spellings address one
// object. Identity multihashes (content inlined in the CID) are refused.
func Normalize(s string) (string, error) {
	if s == "" || len(s) > maxCIDLen {
		return "", ErrInvalidCID
	}
	c, err := cid.Decode(s)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCID, err)
	}
	dec, err := mh.Decode(c.Hash())
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCID, err)
	}
	if dec.Code == mh.IDENTITY {
		return "", fmt.Errorf("%w: identity multihash", ErrInvalidCID)
	}
	if c.Version() == 0 {
		c = cid.NewCidV1(cid.DagProtobuf, c.Hash())
	}
	return c.Encode(multibase.MustNewEncoder(multibase.Base32)), nil
}

// ObjectKey is the S3 key of a normalized CID, sharded two levels deep on its
// last four characters (the base32 prefix is the same for every CID).
func ObjectKey(norm string) string {
	n := len(norm)
	if n < 4 {
		return norm
	}
	return norm[n-4:n-2] + "/" + norm[n-2:] + "/" + norm
}

// CleanSubpath validates what follows the CID in /ipfs/<cid>/<path> and returns
// it without surrounding slashes; "" means the CID itself. Dot segments, empty
// segments, backslashes and control characters are refused rather than
// resolved, so an object never has two spellings.
func CleanSubpath(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	if len(p) > maxSubpathLen {
		return "", ErrInvalidPath
	}
	segs := strings.Split(p, "/")
	if len(segs) > maxSegments {
		return "", ErrInvalidPath
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", ErrInvalidPath
		}
		for _, r := range s {
			if r < 0x20 || r == 0x7f || r == '\\' {
				return "", ErrInvalidPath
			}
		}
	}
	return p, nil
}

// VerifyRaw checks content against a raw-codec SHA2-256 CID and reports whether
// it could check at all. dag-pb CIDs depend on how the file was chunked when it
// was added to IPFS, so they cannot be checked from the bytes alone.
func VerifyRaw(norm string, r io.Reader) (checked bool, err error) {
	c, err := cid.Decode(norm)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrInvalidCID, err)
	}
	dec, err := mh.Decode(c.Hash())
	if err != nil || c.Type() != cid.Raw || dec.Code != mh.SHA2_256 {
		return false, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return true, err
	}
	if !bytes.Equal(h.Sum(nil), dec.Digest) {
		return true, fmt.Errorf("content does not match %s", norm)
	}
	return true, nil
}
