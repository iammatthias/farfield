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

	mux := routes(site)

	// Bard is otherwise database-free; this SQLite file exists purely so the
	// pulse collector can roll up request events. A static site must never
	// fail over analytics, so an open error just disables recording.
	var handler http.Handler = mux
	if db, err := store.OpenDB(store.Env("BARD_DB_PATH", "bard.sqlite")); err != nil {
		slog.Warn("pulse recording disabled: could not open database", "err", err)
	} else {
		defer db.Close()
		rec := pulse.New(db, "bard")
		defer rec.Close()
		handler = rec.Wrap(handler)
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
		// The document is checked first, because it is the thing that carries
		// the versioned links — it must never be the thing pinned for a year.
		// With the ?v= case ahead of it, requesting /index.html?v=anything
		// returned the shell as immutable, freezing a visitor on that build
		// until their cache expired; a shared link could do it deliberately.
		case r.URL.Path == "/" || r.URL.Path == "/index.html":
			w.Header().Set("Cache-Control", "no-cache")
		case r.URL.Query().Get("v") != "":
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		next.ServeHTTP(w, r)
	})
}

// routes builds the mux.
//
// Extracted from main so a test can construct it. ServeMux validates route
// patterns at registration and panics on a bad one, and while that was inline
// in main() nothing ever exercised it: a mangled pattern passed go build, go
// vet, the whole test suite, CI and the image build, then panicked at
// container start — the first moment anything ran the binary, and well past
// the point where a deploy can be called off. A pattern is only as checked as
// the code path that registers it.
func routes(site fs.FS) *http.ServeMux {
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
	// Ahead of the file server: a single-page site still needs its own robots
	// per hostname, and these are more specific than the "/" catch-all.
	mux.HandleFunc("GET /robots.txt", web.RobotsHandler("/sitemap.xml"))
	mux.HandleFunc("GET /sitemap.xml", web.SitemapHandler("/"))
	// index.html is a static shell with no template pass, so the shared asset
	// version is substituted once here. Ahead of the file server, which would
	// otherwise serve the unstamped bytes.
	if shell, err := fs.ReadFile(site, "index.html"); err == nil {
		stamped := theme.StampAssets(shell)
		serveShell := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(stamped)
		}
		mux.HandleFunc("GET /{$}", serveShell)
		mux.HandleFunc("GET /index.html", serveShell)
	}
	mux.Handle("/", cacheControl(http.FileServerFS(site)))
	return mux
}
