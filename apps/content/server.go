package main

import (
	"database/sql"
	"embed"
	"log/slog"
	"net/http"
	"time"

	"github.com/iammatthias/farfield/lib/keys"
	"github.com/iammatthias/farfield/lib/markdown"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates static
var assets embed.FS

// Server holds the running content service.
type Server struct {
	db            *sql.DB
	auth          *web.Auth
	rd            *web.Renderer
	blobsURL      string // internal blobs service URL — for the upload proxy
	blobsKey      string // blobs API key — kept server-side
	blobsPublic   string // browser-facing blobs URL — injected into the editor
	contentPublic string // browser-facing content URL — injected into the editor

	// fleet search sources — internal URLs + optional read keys
	feedURL          string
	feedReadKey      string
	bookmarksURL     string
	bookmarksReadKey string
	siteURLTmpl      string // public page URL pattern with {collection}/{slug} holes; "" = no view-on-site link

	// md renders markdown bodies for the admin UI — the document preview on
	// the edit page and the editor's live preview endpoint.
	md *markdown.Renderer

	// rl rate-limits the public, ungated single-entry read (the "view source"
	// endpoint) per client IP. Keyed callers are exempt; drafts stay 404 to
	// anonymous callers (only the write key previews them).
	rl *web.RateLimiter

	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder

	// rebuild pokes the website's deploy hook when a write changes what the
	// static build reads; nil when no hook is configured (dev, tests).
	rebuild *rebuildTrigger
}

// publicReadPerMin caps anonymous hits to the public single-entry read endpoint
// per client IP per minute. Keyed callers (e.g. the site's server-side fetches)
// bypass it, so this only throttles unauthenticated "view source" traffic.
const publicReadPerMin = 60

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("CONTENT_DB_PATH", "content.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	tmpl, err := web.ParseTemplates(assets, nil)
	if err != nil {
		return err
	}

	s := &Server{
		db: db,
		auth: &web.Auth{
			DB:           db,
			Password:     store.Env("PASSWORD", ""),
			APIKey:       store.Env("CONTENT_API_KEY", ""),
			ReadKey:      store.Env("CONTENT_READ_KEY", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd: &web.Renderer{Templates: tmpl, AssetVer: theme.Version,
			App: "content", Mark: "co",
			Nav: []web.NavItem{
				{Label: "Dashboard", URL: "/"},
				{Label: "Entries", URL: "/entries"},
				{Label: "Series", URL: "/series"},
				{Label: "Log out", URL: "/logout"},
			},
		},
		blobsURL:      store.Env("BLOBS_URL", "http://127.0.0.1:8789"),
		blobsKey:      store.Env("BLOBS_API_KEY", ""),
		blobsPublic:   store.Env("BLOBS_PUBLIC_URL", "http://127.0.0.1:8789"),
		contentPublic: store.Env("CONTENT_PUBLIC_URL", "http://127.0.0.1:8787"),
		siteURLTmpl:   store.Env("SITE_URL_TEMPLATE", ""), // e.g. https://example.com/{collection}/{slug}

		feedURL:          store.Env("FEED_URL", "http://127.0.0.1:8788"),
		feedReadKey:      store.Env("FEED_READ_KEY", ""),
		bookmarksURL:     store.Env("BOOKMARKS_URL", "http://127.0.0.1:8793"),
		bookmarksReadKey: store.Env("BOOKMARKS_READ_KEY", ""),
	}

	s.md = newRenderer(s.db, s.blobsURL, s.blobsPublic)

	defer keys.Attach(s.auth, "content")() // admin-issued keys, when KEYS_DB_PATH is set

	go s.sweepLoop() // retention is hourly, not once at boot

	s.pulse = pulse.New(s.db, "content")
	defer s.pulse.Close()

	// The website is a static build; without this it does not learn that
	// anything was published until the next code push. The URL is a bearer
	// secret and lives only in the deployment environment.
	s.rebuild = newRebuildTrigger(store.Env("CF_DEPLOY_HOOK_URL", ""))
	if s.rebuild == nil {
		slog.Info("no CF_DEPLOY_HOOK_URL — publishing will not trigger a site rebuild")
	}
	defer s.rebuild.Close()

	return web.Serve(host, port, web.MaxBodyExcept(s.routes(), web.DefaultMaxBody,
		web.PathPrefixSkipper("/embed")))
}

func (s *Server) routes() http.Handler {
	if s.rl == nil {
		s.rl = web.NewRateLimiter(publicReadPerMin, time.Minute)
	}
	mux := http.NewServeMux()

	// HTML admin UI — session-gated.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleDashboard))
	mux.HandleFunc("GET /collections/new", s.auth.RequireSession(s.handleNewCollection))
	mux.HandleFunc("POST /collections", s.auth.RequireSession(s.handleCreateCollection))
	mux.HandleFunc("GET /collections/{slug}/edit", s.auth.RequireSession(s.handleEditCollection))
	mux.HandleFunc("POST /collections/{slug}", s.auth.RequireSession(s.handleUpdateCollection))
	mux.HandleFunc("POST /collections/{slug}/delete", s.auth.RequireSession(s.handleDeleteCollection))
	mux.HandleFunc("GET /entries", s.auth.RequireSession(s.handleEntries))
	mux.HandleFunc("GET /entries/new", s.auth.RequireSession(s.handleNewEntry))
	mux.HandleFunc("GET /entries/trash", s.auth.RequireSession(s.handleTrash))
	mux.HandleFunc("GET /search", s.auth.RequireSession(s.handleFleetSearchPage))
	mux.HandleFunc("GET /fleet-search-data", s.auth.RequireSession(s.handleFleetSearchData))
	mux.HandleFunc("POST /entries", s.auth.RequireSession(s.handleCreateEntry))
	mux.HandleFunc("GET /entries/{slug}/edit", s.auth.RequireSession(s.handleEditEntry))
	mux.HandleFunc("POST /entries/{slug}", s.auth.RequireSession(s.handleUpdateEntry))
	mux.HandleFunc("POST /entries/{slug}/delete", s.auth.RequireSession(s.handleDeleteEntry))
	mux.HandleFunc("POST /entries/{slug}/restore", s.auth.RequireSession(s.handleRestoreEntry))
	mux.HandleFunc("POST /entries/{slug}/destroy", s.auth.RequireSession(s.handleDestroyEntry))
	mux.HandleFunc("POST /entries/{slug}/revisions/{id}/restore", s.auth.RequireSession(s.handleRestoreRevision))
	mux.HandleFunc("GET /series", s.auth.RequireSession(s.handleSeriesList))
	mux.HandleFunc("GET /series/new", s.auth.RequireSession(s.handleNewSeries))
	mux.HandleFunc("POST /series", s.auth.RequireSession(s.handleCreateSeries))
	mux.HandleFunc("GET /series/{slug}/edit", s.auth.RequireSession(s.handleEditSeries))
	mux.HandleFunc("POST /series/{slug}", s.auth.RequireSession(s.handleUpdateSeries))
	mux.HandleFunc("POST /series/{slug}/delete", s.auth.RequireSession(s.handleDeleteSeries))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// JSON read API — bearer-token-gated when CONTENT_READ_KEY is set (the
	// write CONTENT_API_KEY is also accepted, and unlocks drafts for preview).
	// /status stays public so the healthcheck and uptime probes never need a
	// token. Published content only, unless the request carries the write key.
	//
	// A single PUBLISHED entry by slug is the exception: it is public (so the
	// site's "view source" links open in a browser) but rate-limited per client
	// IP, with keyed callers exempt. Draft protection is unchanged — handleAPIEntry
	// 404s a draft unless the write key is present, so anonymous callers can never
	// see one. The enumerating lists, collections, and series stay token-gated
	// (a series can back an unpublished entry).
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/collections", s.auth.RequireReadKey(s.handleAPICollections))
	mux.HandleFunc("GET /api/entries", s.auth.RequireReadKey(s.handleAPIEntries))
	mux.HandleFunc("GET /api/entries/{slug}", web.RateLimit(s.rl, s.auth.HasReadKey, s.handleAPIEntry))
	mux.HandleFunc("GET /api/series", s.auth.RequireReadKey(s.handleAPISeries))
	mux.HandleFunc("GET /api/series/{slug}", s.auth.RequireReadKey(s.handleAPISeriesOne))

	// JSON write API — API-key-gated.
	mux.HandleFunc("POST /api/entries", s.auth.RequireAPIKey(s.handleAPICreateEntry))
	mux.HandleFunc("PUT /api/entries/{slug}", s.auth.RequireAPIKey(s.handleAPIUpdateEntry))
	mux.HandleFunc("DELETE /api/entries/{slug}", s.auth.RequireAPIKey(s.handleAPIDeleteEntry))
	mux.HandleFunc("POST /api/series", s.auth.RequireAPIKey(s.handleAPICreateSeries))

	// Editor embedding — session-gated proxy so service keys stay server-side.
	// The list reads (blob gallery, series picker) proxy the now-token-gated
	// sibling APIs so the editor page never needs a read token.
	mux.HandleFunc("POST /preview", s.auth.RequireSession(s.handlePreview))
	mux.HandleFunc("POST /editdoc", s.auth.RequireSession(s.handleEditdoc))
	// Assist proposes tags and an excerpt for the open draft. Session-gated
	// like the rest of the editor: it spends model credit, so it is a thing
	// the author does, not a thing the API offers.
	mux.HandleFunc("POST /assist", s.auth.RequireSession(s.handleAssist))
	mux.HandleFunc("POST /embed/blob", s.auth.RequireSession(s.handleEmbedBlob))
	mux.HandleFunc("POST /embed/series", s.auth.RequireSession(s.handleEmbedSeries))
	mux.HandleFunc("GET /embed/blobs", s.auth.RequireSession(s.handleEmbedBlobsList))
	mux.HandleFunc("GET /embed/series", s.auth.RequireSession(s.handleEmbedSeriesList))

	// Shared theme stylesheet and editor script.
	mux.HandleFunc("GET /static/fonts.css", theme.FontsHandler())
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())
	mux.HandleFunc("GET /static/editor.js", theme.EditorJSHandler())
	mux.HandleFunc("GET /static/band.js", theme.BandJSHandler())

	// App-local static assets. The vendored semantic-search engine is
	// versioned by URL (?v=) and immutable; everything else revalidates.
	// The exact patterns above outrank this subtree.
	mux.Handle("GET /static/", staticHandler())

	// Search corpus for the entries page — session-gated like the page.
	mux.HandleFunc("GET /search-data", s.auth.RequireSession(s.handleSearchData))

	// Everything content serves is text — HTML, JSON — so Gzip wraps the
	// whole mux. Logging sits outside so the recorded status is the final one;
	// pulse traffic recording sits innermost so logged timings stay real. The
	// rebuild trigger sits innermost of all: it must see the database as each
	// handler left it, and its fingerprinting should not be counted as request
	// time by pulse.
	return web.CORS(web.LogRequests(web.Gzip(s.pulse.Wrap(s.rebuild.Wrap(s.db, mux)))),
		"GET", "POST", "PUT", "DELETE", "OPTIONS")
}
