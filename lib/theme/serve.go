package theme

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/iammatthias/farfield/lib/cid"
)

// Version fingerprints the shared assets. Apps link the stylesheet as
// /static/styles.css?v={{.AssetVer}} with AssetVer set to this value, which
// makes the immutable Cache-Control below safe: a theme change changes the
// URL, so clients can cache the old one forever.
var Version = cid.Of([]byte(CSS + EditorJS))[:16]

// CSSHandler serves the shared stylesheet with immutable caching and a
// precomputed gzip variant.
func CSSHandler() http.HandlerFunc {
	return assetHandler("text/css; charset=utf-8", CSS, "theme.css")
}

// EditorJSHandler serves the shared editor script the same way.
func EditorJSHandler() http.HandlerFunc {
	return assetHandler("text/javascript; charset=utf-8", EditorJS, "editor.js")
}

func assetHandler(contentType, body, filename string) http.HandlerFunc {
	var once sync.Once
	var gzipped []byte
	etag := `"` + Version + `"`
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		// Dev mode: FARFIELD_DEV_THEME points at lib/theme — serve the file
		// from disk uncached so edits show on reload without a rebuild.
		if dir := os.Getenv("FARFIELD_DEV_THEME"); dir != "" {
			if b, err := os.ReadFile(filepath.Join(dir, filename)); err == nil {
				h.Set("Cache-Control", "no-cache")
				w.Write(b)
				return
			}
		}
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("ETag", etag)
		h.Set("Vary", "Accept-Encoding")
		if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			once.Do(func() {
				var buf bytes.Buffer
				zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
				zw.Write([]byte(body))
				zw.Close()
				gzipped = buf.Bytes()
			})
			h.Set("Content-Encoding", "gzip")
			w.Write(gzipped)
			return
		}
		w.Write([]byte(body))
	}
}
