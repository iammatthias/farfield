// Command bard serves the bard browser inference app: a small GPT trained on
// Shakespeare whose weights are sealed onchain and verified in the browser.
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
	mux.Handle("/", http.FileServerFS(site))

	if err := web.Serve(host, port, web.LogRequests(web.Gzip(mux))); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}
