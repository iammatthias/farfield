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
	"strconv"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
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
	{"index", "./", "Docs", "Farfield Systems — Docs"},
	{"apex", "apex", "Apex", "Apex — Farfield Docs"},
	{"content", "content", "Content", "Content — Farfield Docs"},
	{"feed", "feed", "Feed", "Feed — Farfield Docs"},
	{"blobs", "blobs", "Blobs", "Blobs — Farfield Docs"},
	{"library", "library", "Library", "Library — Farfield Docs"},
	{"daily", "daily", "Daily", "Daily — Farfield Docs"},
	{"bookmarks", "bookmarks", "Bookmarks", "Bookmarks — Farfield Docs"},
	{"qr", "qr", "QR", "QR — Farfield Docs"},
	{"keys", "keys", "Keys", "Keys — Farfield Docs"},
	{"sideload", "sideload", "Sideload", "Sideload — Farfield Docs"},
	{"bard", "bard", "Bard", "Bard — Farfield Docs"},
	{"dead-presidents", "dead-presidents", "Dead Presidents", "Dead Presidents — Farfield Docs"},
	{"backup", "backup", "Backup", "Backup — Farfield Docs"},
	{"skills", "skills", "Skills", "Skills — Farfield Docs"},
}

// legacyDocs maps renamed docs pages to their current homes — old URLs keep
// working with a permanent redirect.
var legacyDocs = map[string]string{
	"calendar": "daily",
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

	// Apex is otherwise database-free; this SQLite file exists purely so the
	// pulse collector can roll up request events. A static site must never
	// fail over analytics, so an open error just disables recording.
	if db, err := store.OpenDB(store.Env("APEX_DB_PATH", "apex.sqlite")); err != nil {
		slog.Warn("pulse recording disabled: could not open database", "err", err)
	} else {
		defer db.Close()
		rec := pulse.New(db, "apex")
		defer rec.Close()
		handler = rec.Wrap(handler)
	}

	if err := web.Serve(host, port, web.LogRequests(web.Gzip(handler))); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

// page is a pre-rendered response: every apex page is fully known at startup,
// so it renders once into bytes and serves from memory with its content ETag.
type page struct {
	body []byte
	etag string // first 16 chars of the body's CID
}

// serve writes the pre-rendered page with validators. Clients cache for five
// minutes and then revalidate by ETag, which a redeploy with new content busts.
func (p page) serve(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("ETag", `"`+p.etag+`"`)
	h.Set("Cache-Control", "public, max-age=300")
	if web.ETagMatch(r, p.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(p.body)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(p.body)
	}
}

// renderDocs executes the shared layout over each docs page once, at startup.
func renderDocs() (map[string]page, error) {
	pages := make(map[string]page, len(docPages))
	for _, d := range docPages {
		t, err := template.New(d.Key).ParseFS(tmplFS,
			"templates/docs/layout.html", "templates/docs/"+d.Key+".html")
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := t.ExecuteTemplate(&buf, "layout",
			pageData{Title: d.Title, Active: d.Key, Nav: docPages, AssetVer: theme.Version}); err != nil {
			return nil, err
		}
		body := buf.Bytes()
		pages[d.Key] = page{body: body, etag: cid.Of(body)[:16]}
	}
	return pages, nil
}

// cacheStatic wraps the embedded file server with Cache-Control. Embedded
// files have a zero ModTime, so without this no validator is ever sent and
// every visit refetches. Versioned assets (the screenshots under /docs/assets
// and the ?v= stylesheet) cache forever — a content change changes the URL —
// and everything else revalidates hourly.
func cacheStatic(files http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/docs/assets/") ||
			(r.URL.Path == "/docs/style.css" && r.URL.Query().Has("v")) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		files.ServeHTTP(w, r)
	})
}

// routes builds the apex handler: docs pages pre-rendered over the shared
// layout, the shared theme stylesheet, and the static landing page + assets.
func routes() (http.Handler, error) {
	site, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}

	pages, err := renderDocs()
	if err != nil {
		return nil, err
	}

	// The landing page is static too — pre-load it for the same validators.
	landingBody, err := fs.ReadFile(site, "index.html")
	if err != nil {
		return nil, err
	}
	landing := page{body: landingBody, etag: cid.Of(landingBody)[:16]}

	files := cacheStatic(http.FileServerFS(site))
	mux := http.NewServeMux()

	// Shared farfield theme at the canonical path; docs layer style.css over it.
	mux.Handle("GET /static/styles.css", theme.CSSHandler())

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		web.WriteJSON(w, http.StatusOK, map[string]any{"service": "apex", "ok": true})
	})

	// Docs: /docs/ is the index; /docs/<page> serves a pre-rendered page; the
	// legacy .html forms 301 to the canonical extensionless URL; other
	// single-segment paths under /docs (style.css) fall through to the assets.
	mux.HandleFunc("GET /docs/{$}", func(w http.ResponseWriter, r *http.Request) {
		pages["index"].serve(w, r)
	})
	mux.HandleFunc("GET /docs/{page}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("page")
		stem := strings.TrimSuffix(name, ".html")
		if to, ok := legacyDocs[stem]; ok { // renamed pages — e.g. calendar → daily
			http.Redirect(w, r, "/docs/"+to, http.StatusMovedPermanently)
			return
		}
		if _, ok := pages[stem]; ok {
			switch {
			case stem == "index": // /docs/index(.html) → the directory index
				http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
			case stem != name: // /docs/<page>.html → /docs/<page>
				http.Redirect(w, r, "/docs/"+stem, http.StatusMovedPermanently)
			default:
				pages[stem].serve(w, r)
			}
			return
		}
		if stem != name { // an unknown doc — not an asset
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})

	// Landing page from memory; nested doc assets (/docs/assets/*) from embed.
	mux.HandleFunc("GET /{$}", landing.serve)
	mux.Handle("/", files)
	return mux, nil
}
