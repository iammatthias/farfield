// Command dead-presidents serves the Dead Presidents GPT browser inference app:
// a character-level GPT trained on US presidential speeches whose weights run
// entirely client-side in a Web Worker. The server only ships static assets and
// the shared farfield theme; all inference happens in the browser.
package main

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"os"

	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed web
var webFS embed.FS

// cacheControl sets Cache-Control for the embedded assets. Requests carrying
// the ?v= cache-busting version (scripts from index.html, and the model files
// the worker fetches with its own version — see web/worker.js) are immutable:
// a content change always ships under a new version, so browsers may cache the
// 1.86MB model forever. The HTML entry point revalidates on every visit so a
// version bump is picked up immediately; anything unversioned gets a short TTL.
func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("v") != "":
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case r.URL.Path == "/" || r.URL.Path == "/index.html":
			w.Header().Set("Cache-Control", "no-cache")
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	_ = store.LoadEnv()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	port := store.Env("DEAD_PRESIDENTS_PORT", "8796")
	if len(os.Args) > 1 && os.Args[1] == "health" {
		// Probes /status — backs the Docker healthcheck (distroless: no curl).
		os.Exit(web.Health(port))
	}
	host := store.Env("HOST", "127.0.0.1")

	site, err := fs.Sub(webFS, "web")
	if err != nil {
		slog.Error("loading embedded site", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		web.WriteJSON(w, http.StatusOK, map[string]any{"service": "dead-presidents", "ok": true})
	})
	// Serve the shared farfield theme at the same path the other apps use, so
	// dead-presidents inherits the canonical stylesheet instead of a local copy
	// that drifts.
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())
	mux.Handle("/", cacheControl(http.FileServerFS(site)))

	// Dead-presidents is otherwise database-free; this SQLite file exists
	// purely so the pulse collector can roll up request events. A static site
	// must never fail over analytics, so an open error just disables recording.
	var handler http.Handler = mux
	if db, err := store.OpenDB(store.Env("DEAD_PRESIDENTS_DB_PATH", "dead-presidents.sqlite")); err != nil {
		slog.Warn("pulse recording disabled: could not open database", "err", err)
	} else {
		defer db.Close()
		rec := pulse.New(db, "dead-presidents")
		defer rec.Close()
		handler = rec.Wrap(handler)
	}

	// Gzip helps the text assets and model.json; model.bin is served as
	// application/octet-stream, which is not in the gzip allowlist and passes
	// through untouched. Logging sits outside so the recorded status is final.
	if err := web.Serve(host, port, web.LogRequests(web.Gzip(handler))); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}
