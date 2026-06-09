// Command apex serves the farfield apex site — farfield.systems, the public
// face of the project: a static landing page and the documentation. Assets are
// embedded into the binary, so the service is self-contained: no database, no
// volume, no external files. The docs render from one shared layout
// (templates/docs/layout.html) over a single nav registry, so the sidebar
// lives in exactly one place.
package main

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed web
var webFS embed.FS

//go:embed templates
var tmplFS embed.FS

// doc is one documentation page: its key (route stem and active marker), the
// relative href used in the sidebar, the sidebar label, and the <title>.
type doc struct{ Key, Href, Label, Title string }

// docPages is the single source of truth for the docs sidebar and routes — add
// a page here and a templates/docs/<key>.html content file, nowhere else.
var docPages = []doc{
	{"index", "index.html", "Docs", "Farfield Systems — Docs"},
	{"apex", "apex.html", "Apex", "Apex — Farfield Docs"},
	{"content", "content.html", "Content", "Content — Farfield Docs"},
	{"feed", "feed.html", "Feed", "Feed — Farfield Docs"},
	{"blobs", "blobs.html", "Blobs", "Blobs — Farfield Docs"},
	{"library", "library.html", "Library", "Library — Farfield Docs"},
	{"calendar", "calendar.html", "Calendar", "Calendar — Farfield Docs"},
	{"bookmarks", "bookmarks.html", "Bookmarks", "Bookmarks — Farfield Docs"},
	{"qr", "qr.html", "QR", "QR — Farfield Docs"},
	{"bard", "bard.html", "Bard", "Bard — Farfield Docs"},
	{"dead-presidents", "dead-presidents.html", "Dead Presidents", "Dead Presidents — Farfield Docs"},
	{"backup", "backup.html", "Backup", "Backup — Farfield Docs"},
	{"skills", "skills.html", "Skills", "Skills — Farfield Docs"},
}

// pageData is the template context for a rendered docs page. AssetVer
// fingerprints the shared theme stylesheet so it can cache as immutable.
type pageData struct {
	Title    string
	Active   string
	Nav      []doc
	AssetVer string
}

func main() {
	_ = store.LoadEnv() // finds the root .env, wherever the app is run from
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// "health" probes the running server's /status for Docker healthchecks.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(web.Health(store.Env("APEX_PORT", "8790")))
	}

	host := store.Env("HOST", "127.0.0.1")
	port := store.Env("APEX_PORT", "8790")

	handler, err := routes()
	if err != nil {
		slog.Error("building routes", "err", err)
		os.Exit(1)
	}

	if err := web.Serve(host, port, web.LogRequests(web.Gzip(handler))); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

// routes builds the apex handler: docs pages rendered over the shared layout,
// the shared theme stylesheet, and the static landing page + doc assets.
func routes() (http.Handler, error) {
	site, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}

	// Parse the layout with each page's content into its own template set.
	pages := make(map[string]*template.Template, len(docPages))
	titles := make(map[string]string, len(docPages))
	for _, d := range docPages {
		t, err := template.New(d.Key).ParseFS(tmplFS,
			"templates/docs/layout.html", "templates/docs/"+d.Key+".html")
		if err != nil {
			return nil, err
		}
		pages[d.Key] = t
		titles[d.Key] = d.Title
	}

	render := func(w http.ResponseWriter, key string) {
		var buf bytes.Buffer
		if err := pages[key].ExecuteTemplate(&buf, "layout",
			pageData{Title: titles[key], Active: key, Nav: docPages, AssetVer: theme.Version}); err != nil {
			slog.Error("render doc", "key", key, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = buf.WriteTo(w)
	}

	files := http.FileServerFS(site)
	mux := http.NewServeMux()

	// Shared farfield theme at the canonical path; docs layer style.css over it.
	mux.Handle("GET /static/styles.css", theme.CSSHandler())

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		web.WriteJSON(w, http.StatusOK, map[string]any{"service": "apex", "ok": true})
	})

	// Docs: /docs/ is the index; /docs/<page>.html renders a page; other
	// single-segment paths under /docs (style.css) fall through to the assets.
	mux.HandleFunc("GET /docs/{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, "index")
	})
	mux.HandleFunc("GET /docs/{page}", func(w http.ResponseWriter, r *http.Request) {
		page := r.PathValue("page")
		if _, ok := pages[strings.TrimSuffix(page, ".html")]; ok {
			render(w, strings.TrimSuffix(page, ".html"))
			return
		}
		if strings.HasSuffix(page, ".html") { // an unknown doc — not an asset
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})

	// Landing page and nested doc assets (/docs/assets/*).
	mux.Handle("/", files)
	return mux, nil
}
