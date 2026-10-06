package editor

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

//go:embed host.js
var HostJS string

//go:embed host.css
var HostCSS string

//go:embed mount.js
var MountJS string

var (
	buildOnce sync.Once
	wasmBin   []byte
	buildErr  error
	version   string
)

// Wasm returns the assembled module, building it from the .wat sources on
// first use. Assembly takes milliseconds, so there is no committed binary to
// drift from its source.
func Wasm() ([]byte, error) {
	buildOnce.Do(func() {
		wasmBin, buildErr = Build()
		if buildErr != nil {
			return
		}
		h := sha256.New()
		h.Write(wasmBin)
		h.Write([]byte(HostJS))
		h.Write([]byte(HostCSS))
		h.Write([]byte(MountJS))
		h.Write(Dictionary)
		for _, name := range FontSlots {
			b, _ := Font(name)
			h.Write(b)
		}
		version = hex.EncodeToString(h.Sum(nil))[:16]
	})
	return wasmBin, buildErr
}

// Version fingerprints every editor asset, for cache-busting URLs.
func Version() string {
	if _, err := Wasm(); err != nil {
		return "dev"
	}
	return version
}

// FontURLs lists the font asset paths under prefix, in slot order.
func FontURLs(prefix string) []string {
	out := make([]string, len(FontSlots))
	for i, n := range FontSlots {
		out[i] = strings.TrimRight(prefix, "/") + "/fonts/" + n + "?v=" + Version()
	}
	return out
}

// textAssets are the editor assets worth compressing, keyed by the name
// Handler serves them under. Fonts and the wasm module are left alone.
var textAssets = map[string]func() []byte{
	"host.js":        func() []byte { return []byte(HostJS) },
	"host.css":       func() []byte { return []byte(HostCSS) },
	"mount.js":       func() []byte { return []byte(MountJS) },
	"dict/en_US.txt": func() []byte { return Dictionary },
}

var (
	gzOnce   sync.Once
	gzAssets map[string][]byte
)

// gzipped returns the asset's precompressed bytes. The assets are embedded and
// never change while the process runs, so they are compressed once, at best
// compression, rather than by the Gzip middleware on every request — for the
// 1 MB dictionary that was ~11 ms of CPU per load, for the same ~300 KB result.
func gzipped(name string) ([]byte, bool) {
	gzOnce.Do(func() {
		gzAssets = make(map[string][]byte, len(textAssets))
		for n, body := range textAssets {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			_, _ = zw.Write(body())
			_ = zw.Close()
			gzAssets[n] = buf.Bytes()
		}
	})
	b, ok := gzAssets[name]
	return b, ok
}

// acceptsGzip reports whether the client takes a gzip response. A q=0
// refusal is vanishingly rare from the browsers that load the editor and is
// not parsed, matching the fleet's Gzip middleware.
func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// notModified reports whether If-None-Match names this asset version, in
// either encoding. Weak (W/) and listed validators count, as in lib/web's
// ETagMatch — duplicated here because importing lib/web would pull its auth
// and sqlite store into every module that embeds the editor.
func notModified(r *http.Request, version string) bool {
	header := r.Header.Get("If-None-Match")
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, c := range strings.Split(header, ",") {
		c = strings.Trim(strings.TrimPrefix(strings.TrimSpace(c), "W/"), `"`)
		if strings.TrimSuffix(c, "-gz") == version {
			return true
		}
	}
	return false
}

// Handler serves the editor's assets under prefix (e.g. "/static/editor/"):
// editor.wasm, host.js, host.css, mount.js, fonts/*.ttf and the spelling
// dictionary (dict/en_US.txt). A request carrying the
// current ?v= is cached for a year; anything else for five minutes.
//
// Every asset carries the editor Version as its ETag, so a five-minute copy
// revalidates with a bodiless 304 instead of a re-download. The text assets
// go out precompressed (Content-Encoding: gzip) to clients that accept it;
// the fleet's Gzip middleware passes an already-encoded response through
// untouched. The gzip representation's tag is suffixed "-gz": a strong ETag
// names one exact byte sequence, and the two encodings are two.
func Handler(prefix string) http.Handler {
	prefix = strings.TrimRight(prefix, "/") + "/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, prefix)
		var body []byte
		var ctype string
		switch {
		case name == "editor.wasm":
			b, err := Wasm()
			if err != nil {
				http.Error(w, "editor build failed", http.StatusInternalServerError)
				return
			}
			body, ctype = b, "application/wasm"
		case name == "host.js":
			body, ctype = []byte(HostJS), "text/javascript; charset=utf-8"
		case name == "host.css":
			body, ctype = []byte(HostCSS), "text/css; charset=utf-8"
		case name == "mount.js":
			body, ctype = []byte(MountJS), "text/javascript; charset=utf-8"
		case name == "dict/en_US.txt":
			body, ctype = Dictionary, "text/plain; charset=utf-8"
		case strings.HasPrefix(name, "fonts/") && strings.HasSuffix(name, ".ttf"):
			b, err := Font(strings.TrimPrefix(name, "fonts/"))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			body, ctype = b, "font/ttf"
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Query().Get("v") == Version() {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		gz, isText := gzipped(name)
		if isText {
			// the response differs by Accept-Encoding whichever branch runs
			w.Header().Add("Vary", "Accept-Encoding")
		}
		// "dev" means the wasm build failed and is not a real fingerprint; a
		// tag from it could match across builds, so send none.
		if v := Version(); v != "dev" {
			etag := v
			if isText && acceptsGzip(r) {
				etag += "-gz"
			}
			w.Header().Set("ETag", `"`+etag+`"`)
			if notModified(r, v) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		if isText && acceptsGzip(r) {
			w.Header().Set("Content-Encoding", "gzip")
			body = gz
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	})
}
