// Package policy decides which content seapin stores and serves, and sets the
// response headers that keep served bytes inert in a browser.
package policy

import (
	"mime"
	"net/http"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

const (
	// SniffLen is how many leading bytes Classify looks at. It is also
	// mimetype's read limit: mimetype accepts a JSON document cut short only
	// when the input reaches that limit, so the two must stay equal.
	SniffLen = 4096
	// MaxObjectSize is the largest object seapin stores (D3).
	MaxObjectSize = 256 << 20
)

func init() { mimetype.SetLimit(SniffLen) }

// served maps each accepted type, as gabriel-vasile/mimetype names it, to the
// Content-Type the object is stored and served with. Everything else is
// blocked: HTML, JS, XML, PDF, plain text, executables, archives, unknown bytes.
var served = map[string]string{
	"image/png":              "image/png",
	"image/vnd.mozilla.apng": "image/png", // an APNG is a PNG; browsers animate it as image/png
	"image/jpeg":             "image/jpeg",
	"image/gif":              "image/gif",
	"image/webp":             "image/webp",
	"image/avif":             "image/avif",
	"image/bmp":              "image/bmp",
	"image/svg+xml":          "image/svg+xml", // always served with the sandbox CSP (D4)
	"video/mp4":              "video/mp4",
	"video/webm":             "video/webm",
	"video/quicktime":        "video/quicktime",
	"audio/mpeg":             "audio/mpeg",
	"audio/ogg":              "audio/ogg",
	"audio/wav":              "audio/wav",
	"audio/flac":             "audio/flac",
	"audio/aac":              "audio/aac",
	"audio/mp4":              "audio/mp4",
	"audio/x-m4a":            "audio/mp4",
	"model/gltf-binary":      "model/gltf-binary",
	"application/json":       "application/json",
}

var allowed = func() map[string]bool {
	m := make(map[string]bool, len(served))
	for _, ct := range served {
		m[ct] = true
	}
	return m
}()

// Classify sniffs the leading bytes of an object (at least SniffLen of them,
// or the whole object) and returns the Content-Type to store and serve it with.
// ok is false when the type is not accepted; detected is what was found either
// way. Specialised types inherit acceptance from a parent (GeoJSON is JSON).
func Classify(head []byte) (contentType, detected string, ok bool) {
	m := mimetype.Detect(head)
	for t := m; t != nil; t = t.Parent() {
		if ct, found := served[baseType(t.String())]; found {
			return ct, m.String(), true
		}
	}
	return "", m.String(), false
}

// Allowed reports whether a stored Content-Type may be served. It re-checks
// at serve time what ingest decided at store time.
func Allowed(contentType string) bool {
	return allowed[baseType(contentType)]
}

// CSP keeps an object opened directly in a browser inert: no scripts, no
// network beyond the object itself, sandboxed origin. 'self' lets the
// browser's own viewer show the image or play the video it was opened for;
// data: images stay allowed for SVGs that embed rasters. Embedding through
// <img>/<video> on another site is unaffected.
const CSP = "default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; sandbox"

// SetHeaders adds the headers every gateway response carries, errors included.
func SetHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", CSP)
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Set("Access-Control-Allow-Origin", "*")
}

func baseType(ct string) string {
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		return mt
	}
	return strings.ToLower(strings.TrimSpace(ct))
}
