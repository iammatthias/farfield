package editor

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
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

// Handler serves the editor's assets under prefix (e.g. "/static/editor/"):
// editor.wasm, host.js, host.css, mount.js, fonts/*.ttf and the spelling
// dictionary (dict/en_US.txt). A request carrying the
// current ?v= is cached for a year; anything else for five minutes.
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
		w.Write(body)
	})
}
