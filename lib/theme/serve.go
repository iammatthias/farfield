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

// Styles is the stylesheet without its @font-face blocks — the rules that
// actually change when the theme changes.
//
// The faces are vendored as data URIs, which is 82% of the file: 268 KB of
// 327 KB, 219 KB of the 224 KB that goes over the wire gzipped. Keeping them
// in the same asset meant Version hashed them too, so editing one colour
// changed the stylesheet's URL and every client re-downloaded a quarter of a
// megabyte of unchanged font bytes. Split, an ordinary theme edit invalidates
// about 6 KB and the faces stay in cache across every release that does not
// touch them.
var Styles = stripFontFaces(CSS)

// Version fingerprints the shared assets. Apps link the stylesheet as
// /static/styles.css?v={{.AssetVer}} with AssetVer set to this value, which
// makes the immutable Cache-Control below safe: a theme change changes the
// URL, so clients can cache the old one forever.
var Version = cid.Of([]byte(Styles + EditorJS + BandJS))[:16]

// FontsVersion fingerprints the faces alone, so they carry a URL that changes
// only when a font does.
var FontsVersion = cid.Of([]byte(Fonts))[:16]

// CSSHandler serves the shared stylesheet with immutable caching and a
// precomputed gzip variant. The faces come from FontsHandler; every page that
// links this must link that too — lib/web's layout does.
func CSSHandler() http.HandlerFunc {
	return assetHandler("text/css; charset=utf-8", Styles, "theme.css", Version)
}

// EditorJSHandler serves the shared editor script the same way.
func EditorJSHandler() http.HandlerFunc {
	return assetHandler("text/javascript; charset=utf-8", EditorJS, "editor.js", Version)
}

// BandJSHandler serves the meta-band script the same way.
func BandJSHandler() http.HandlerFunc {
	return assetHandler("text/javascript; charset=utf-8", BandJS, "band.js", Version)
}

func assetHandler(contentType, body, filename, version string) http.HandlerFunc {
	var once sync.Once
	var gzipped []byte
	etag := `"` + version + `"`
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		// Dev mode: FARFIELD_DEV_THEME points at lib/theme — serve the file
		// from disk uncached so edits show on reload without a rebuild.
		if dir := os.Getenv("FARFIELD_DEV_THEME"); dir != "" {
			// theme.css on disk is the whole stylesheet; production serves it
			// in two pieces. Both dev assets come from that one file, split the
			// same way — otherwise dev serves the faces twice and any bug in
			// the splitter stays hidden until deploy.
			src, split := filename, func(s string) string { return s }
			switch filename {
			case "theme.css":
				split = stripFontFaces
			case "fonts.css":
				src, split = "theme.css", extractFontFaces
			}
			if b, err := os.ReadFile(filepath.Join(dir, src)); err == nil {
				h.Set("Cache-Control", "no-cache")
				w.Write([]byte(split(string(b))))
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

// Fonts is just the @font-face rules from the stylesheet — the three vendored
// latin-subset faces as data URIs, no layout or colour.
//
// It exists because a page can want the fleet's typography without the fleet's
// component styles. Apex's landing and status pages are hand-styled from the
// brand guide, so linking the whole theme would be wrong; before this they
// reached for Google's CDN instead, which leaked a visitor IP on every request
// to farfield.systems and quietly broke the "self-hosted, no font CDN"
// guarantee the rest of the fleet keeps.
var Fonts = extractFontFaces(CSS)

// FontsHandler serves Fonts with the same immutable caching as the stylesheet,
// under its own version so a theme edit never invalidates the faces.
func FontsHandler() http.HandlerFunc {
	return assetHandler("text/css; charset=utf-8", Fonts, "fonts.css", FontsVersion)
}

// stripFontFaces removes every @font-face{...} block — the inverse of
// extractFontFaces, over the same brace-free blocks.
func stripFontFaces(css string) string {
	var b strings.Builder
	for rest := css; ; {
		i := strings.Index(rest, "@font-face")
		if i < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i])
		rest = rest[i:]
		close := strings.Index(rest, "}")
		if close < 0 {
			b.WriteString(rest)
			break
		}
		rest = rest[close+1:]
		rest = strings.TrimPrefix(rest, "\n")
	}
	return b.String()
}

// extractFontFaces pulls every @font-face{...} block out of a stylesheet.
// The blocks contain no nested braces, so brace counting is unnecessary.
func extractFontFaces(css string) string {
	var b strings.Builder
	for rest := css; ; {
		i := strings.Index(rest, "@font-face")
		if i < 0 {
			break
		}
		rest = rest[i:]
		open := strings.Index(rest, "{")
		close := strings.Index(rest, "}")
		if open < 0 || close < open {
			break
		}
		b.WriteString(rest[:close+1])
		b.WriteByte('\n')
		rest = rest[close+1:]
	}
	return b.String()
}

// assetVersionToken is the placeholder a static HTML shell carries where a
// template would write {{.AssetVer}}.
const assetVersionToken = "__THEME_VERSION__"

// StampAssets substitutes the real asset version into a static HTML shell.
//
// Apps that render through lib/web's shared layout get cache-busting for free.
// The handful that ship a pre-built index.html — apex's landing page among
// them — have no template pass, so they had hand-written version strings that
// never tracked the theme. Since the theme is served
// `immutable, max-age=31536000`, a stale hand-written string meant a fleet-wide
// theme change could not reach a warm cache for a year. Call this once at
// startup on any embedded shell that links a shared asset.
func StampAssets(html []byte) []byte {
	return bytes.ReplaceAll(html, []byte(assetVersionToken), []byte(Version))
}
