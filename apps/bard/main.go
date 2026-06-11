// Command bard serves the bard browser inference app: a small GPT trained on
// Shakespeare whose weights are sealed onchain and verified in the browser.
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

func main() {
	_ = store.LoadEnv()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(web.Health(store.Env("BARD_PORT", "8795")))
	}

	host := store.Env("HOST", "127.0.0.1")
	port := store.Env("BARD_PORT", "8795")

	site, err := fs.Sub(webFS, "web")
	if err != nil {
		slog.Error("loading embedded site", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	// Serve the shared farfield theme at the same path the other apps use, so
	// bard inherits the canonical stylesheet instead of a local copy that drifts.
	mux.Handle("GET /static/styles.css", theme.CSSHandler())
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		web.WriteJSON(w, http.StatusOK, struct {
			Service string `json:"service"`
			OK      bool   `json:"ok"`
		}{Service: "bard", OK: true})
	})
	mux.Handle("/", cacheControl(http.FileServerFS(site)))

	// Bard is otherwise database-free; this SQLite file exists purely so the
	// pulse collector can roll up request events. A static site must never
	// fail over analytics, so an open error just disables recording.
	var handler http.Handler = mux
	if db, err := store.OpenDB(store.Env("BARD_DB_PATH", "bard.sqlite")); err != nil {
		slog.Warn("pulse recording disabled: could not open database", "err", err)
	} else {
		defer db.Close()
		handler = pulse.Middleware(db, "bard")(handler)
	}

	if err := web.Serve(host, port, web.LogRequests(web.Gzip(handler))); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

// cacheControl sets cache headers for the embedded assets: versioned URLs
// (?v=...) are content-addressed by the ASSET_VERSION bump convention and can
// be cached forever, the HTML shell must revalidate so a deploy is picked up
// immediately, and anything else gets a modest TTL.
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
