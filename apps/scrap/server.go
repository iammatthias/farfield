package main

import (
	"database/sql"
	"embed"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/keys"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

// maxPasteBytes bounds a paste body — text only, and 2 MiB of text is a very
// long paste.
const maxPasteBytes = 2 << 20

// composeLangs is the curated lang select on the compose form. The API
// accepts any chroma lexer name; unknown values render plain.
var composeLangs = []string{
	"bash", "c", "cpp", "css", "diff", "dockerfile", "go", "html", "java",
	"javascript", "json", "kotlin", "markdown", "python", "ruby", "rust",
	"sql", "swift", "toml", "typescript", "yaml",
}

// publicReadPerMin caps anonymous reads of a paste per client IP per minute.
// Rendering runs chroma over the body — up to 2 MiB — so this is a CPU bound,
// not a courtesy. A human reads one paste at a time; a scraper does not.
// Keyed callers are exempt, so the terminal API is unaffected.
const publicReadPerMin = 60

// Server holds the running scrap service.
type Server struct {
	db        *sql.DB
	auth      *web.Auth
	rd        *web.Renderer
	publicURL string           // absolute base for URLs the API returns
	limiter   *web.FailLimiter // failed token attempts, per IP+paste

	// rl bounds the anonymous read paths. Rendering a paste runs chroma over
	// up to 2 MiB and /pastes reads bodies, so an unthrottled loop against one
	// public URL burns CPU and DB reads with no ceiling. Keyed callers exempt.
	rl        *web.RateLimiter
	chromaCSS template.CSS // highlight stylesheet, embedded into view pages

	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder
}

// run wires up dependencies and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("SCRAP_DB_PATH", "scrap.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	s := newServer(db,
		store.Env("PASSWORD", ""),
		store.Env("SCRAP_API_KEY", ""),
		store.Env("COOKIE_SECURE", "false") == "true",
		store.Env("SCRAP_PUBLIC_URL", "http://"+net.JoinHostPort("127.0.0.1", port)))
	if err := s.parseTemplates(); err != nil {
		return err
	}

	// Expiry is enforced lazily on read; the sweep keeps the table itself
	// from accumulating dead rows nobody reads.
	go s.sweepLoop()

	defer keys.Attach(s.auth, "scrap")() // admin-issued keys, when KEYS_DB_PATH is set

	s.pulse = pulse.New(s.db, "scrap")
	defer s.pulse.Close()
	return web.Serve(host, port, web.MaxBody(s.routes(), web.DefaultMaxBody))
}

// newServer builds a Server without templates (parseTemplates) or routes —
// split out so tests can assemble one against a temp database.
func newServer(db *sql.DB, password, apiKey string, cookieSecure bool, publicURL string) *Server {
	return &Server{
		db: db,
		auth: &web.Auth{
			DB:           db,
			Password:     password,
			APIKey:       apiKey,
			CookieSecure: cookieSecure,
		},
		publicURL: strings.TrimRight(publicURL, "/"),
		limiter:   web.NewFailLimiter(5, time.Minute),
		chromaCSS: highlightCSS(),
	}
}

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		"relAge":   relAge,
		"ttl":      ttlText,
		"sizeText": sizeText,
	}
	tmpl, err := web.ParseTemplates(assets, funcs)
	if err != nil {
		return err
	}
	s.rd = &web.Renderer{Templates: tmpl, AssetVer: theme.Version, Funcs: funcs,
		App: "scrap", Mark: "sc",
		Nav: []web.NavItem{
			{Label: "Compose", URL: "/"},
			{Label: "Manage", URL: "/manage"},
			{Label: "Public", URL: "/pastes"},
			{Label: "Log out", URL: "/logout"},
		},
	}
	return nil
}

func (s *Server) routes() http.Handler {
	if s.rl == nil {
		s.rl = web.NewRateLimiter(publicReadPerMin, time.Minute)
	}
	mux := http.NewServeMux()

	// Author UI — session-gated. Compose lives at /; manage is the table.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleCompose))
	mux.HandleFunc("POST /pastes", s.auth.RequireSession(s.handleCreate))
	mux.HandleFunc("GET /manage", s.auth.RequireSession(s.handleManage))
	mux.HandleFunc("POST /pastes/{id}/delete", s.auth.RequireSession(s.handleDelete))
	mux.HandleFunc("POST /pastes/expired/delete", s.auth.RequireSession(s.handleDeleteExpired))

	// Token lifecycle — roll replaces, set attaches, remove deletes. Roll and
	// set surface the fresh secret on the shown-once confirmation page.
	mux.HandleFunc("POST /pastes/{id}/token/roll", s.auth.RequireSession(s.handleTokenRoll))
	mux.HandleFunc("POST /pastes/{id}/token/set", s.auth.RequireSession(s.handleTokenSet))
	mux.HandleFunc("POST /pastes/{id}/token/remove", s.auth.RequireSession(s.handleTokenRemove))

	// Login.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// Public reads. Literal /pastes outranks the /{id} wildcard in ServeMux
	// precedence, as do /login, /status, and /static/*.
	mux.HandleFunc("GET /pastes", web.RateLimit(s.rl, s.auth.HasReadKey, s.handlePublicIndex))
	mux.HandleFunc("GET /{id}", web.RateLimit(s.rl, s.auth.HasReadKey, s.handleView))
	mux.HandleFunc("GET /{id}/raw", web.RateLimit(s.rl, s.auth.HasReadKey, s.handleRaw))
	mux.HandleFunc("POST /{id}/unlock", s.handleUnlock)

	// Terminal API — raw text in, a URL out.
	mux.HandleFunc("POST /api/pastes", s.auth.RequireAPIKey(s.handleAPICreate))
	mux.HandleFunc("DELETE /api/pastes/{id}", s.auth.RequireAPIKey(s.handleAPIDelete))
	mux.HandleFunc("POST /api/pastes/{id}/token/roll", s.auth.RequireAPIKey(s.handleAPITokenRoll))
	mux.HandleFunc("DELETE /api/pastes/{id}/token", s.auth.RequireAPIKey(s.handleAPITokenRemove))

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// Everything scrap serves is text, so Gzip wraps the whole mux. Logging
	// sits outside so the recorded status is the final one; pulse traffic
	// recording sits innermost so logged timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(s.pulse.Wrap(mux))),
		"GET", "POST", "DELETE", "OPTIONS")
}

// sweepLoop deletes expired pastes hourly (and once at startup).
func (s *Server) sweepLoop() {
	for {
		if n, err := deleteExpiredPastes(s.db); err != nil {
			slog.Warn("expiry sweep failed", "err", err)
		} else if n > 0 {
			slog.Info("expiry sweep", "deleted", n)
		}
		time.Sleep(time.Hour)
	}
}
