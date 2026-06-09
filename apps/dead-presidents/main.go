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

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed web
var webFS embed.FS

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
	mux.Handle("/", http.FileServerFS(site))

	// Gzip helps the text assets and model.json; model.bin is served as
	// application/octet-stream, which is not in the gzip allowlist and passes
	// through untouched. Logging sits outside so the recorded status is final.
	if err := web.Serve(host, port, web.LogRequests(web.Gzip(mux))); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}
