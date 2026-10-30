package policy

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

var (
	pngSig  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")
	gifSig  = []byte("GIF89a\x01\x00\x01\x00")
	jpegSig = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	mp4Sig  = []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2")
	glbSig  = []byte("glTF\x02\x00\x00\x00\x00\x01\x00\x00")
	svgDoc  = []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"><script>alert(1)</script></svg>`)
	jsonDoc = []byte(`{"name":"Asset #1","image":"ipfs://bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi","properties":{"file_url":"ipfs://x"}}`)
)

func TestClassifyAccepts(t *testing.T) {
	cases := map[string]struct {
		head []byte
		want string
	}{
		"png":  {pngSig, "image/png"},
		"gif":  {gifSig, "image/gif"},
		"jpeg": {jpegSig, "image/jpeg"},
		"mp4":  {mp4Sig, "video/mp4"},
		"glb":  {glbSig, "model/gltf-binary"},
		"svg":  {svgDoc, "image/svg+xml"},
		"json": {jsonDoc, "application/json"},
		// a JSON object longer than SniffLen, cut at the sniff window
		"long json": {append([]byte(`{"description":"`+strings.Repeat("x", 4*SniffLen)), []byte(`"}`)...)[:SniffLen], "application/json"},
	}
	for name, c := range cases {
		ct, detected, ok := Classify(c.head)
		if !ok || ct != c.want {
			t.Errorf("%s: got %q ok=%v (detected %s), want %q", name, ct, ok, detected, c.want)
		}
	}
}

func TestClassifyBlocks(t *testing.T) {
	for name, head := range map[string][]byte{
		"html":       []byte("<!DOCTYPE html><html><body><script>alert(1)</script></body></html>"),
		"html bare":  []byte("  \n<html><script src=x></script>"),
		"javascript": []byte("#!/usr/bin/env node\nconsole.log(1)"),
		"plain text": []byte("just some words\n"),
		"xml":        []byte(`<?xml version="1.0"?><rss version="2.0"></rss>`),
		"pdf":        []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n"),
		"zip":        []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00"),
		"gzip":       []byte("\x1f\x8b\x08\x00\x00\x00\x00\x00"),
		"elf":        []byte("\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00"),
		"wasm":       []byte("\x00asm\x01\x00\x00\x00"),
		"unknown":    bytes.Repeat([]byte{0x01, 0xfe}, 64),
	} {
		if ct, detected, ok := Classify(head); ok {
			t.Errorf("%s: accepted as %s (detected %s)", name, ct, detected)
		}
	}
}

func TestAllowed(t *testing.T) {
	for _, ct := range []string{"image/png", "IMAGE/PNG", "application/json; charset=utf-8", "image/svg+xml"} {
		if !Allowed(ct) {
			t.Errorf("Allowed(%q) = false", ct)
		}
	}
	for _, ct := range []string{"", "text/html", "application/javascript", "application/pdf", "text/plain", "image/vnd.mozilla.apng"} {
		if Allowed(ct) {
			t.Errorf("Allowed(%q) = true", ct)
		}
	}
}

func TestSetHeaders(t *testing.T) {
	h := http.Header{}
	SetHeaders(h)
	if h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("missing protective headers: %v", h)
	}
}
