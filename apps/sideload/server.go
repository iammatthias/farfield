package main

import (
	"database/sql"
	"embed"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"regexp"
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

// maxIPABytes bounds an upload. Ad-hoc app archives run tens to a few hundred
// MiB; 2 GiB is generous headroom and rejects a runaway stream.
const maxIPABytes = 2 << 30

// Server holds the running sideload service.
type Server struct {
	db        *sql.DB
	auth      *web.Auth
	rd        *web.Renderer
	blobs     *blobStore
	publicURL string           // absolute HTTPS base for manifest/ipa/icon URLs
	limiter   *web.FailLimiter // failed token lookups, per client IP
	ownerUDID string           // SIDELOAD_OWNER_UDID — kept in every app's whitelist

	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder
}

// run wires up dependencies and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("SIDELOAD_DB_PATH", "sideload.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	blobs, err := openBlobStore()
	if err != nil {
		return err
	}

	s := newServer(db, blobs,
		store.Env("PASSWORD", ""),
		store.Env("SIDELOAD_API_KEY", ""),
		store.Env("COOKIE_SECURE", "false") == "true",
		store.Env("SIDELOAD_PUBLIC_URL", "http://"+net.JoinHostPort("127.0.0.1", port)))
	if owner, ok := normalizeUDID(store.Env("SIDELOAD_OWNER_UDID", "")); ok {
		s.ownerUDID = owner
		slog.Info("owner device pinned to every app whitelist")
	} else if raw := store.Env("SIDELOAD_OWNER_UDID", ""); raw != "" {
		slog.Warn("SIDELOAD_OWNER_UDID is not a valid UDID — ignoring", "value", raw)
	}
	if err := s.parseTemplates(); err != nil {
		return err
	}

	go s.sweepLoop()

	defer keys.Attach(s.auth, "sideload")() // admin-issued keys, when KEYS_DB_PATH is set

	s.pulse = pulse.New(s.db, "sideload")
	defer s.pulse.Close()
	return web.Serve(host, port, web.MaxBodyExcept(s.routes(), web.DefaultMaxBody,
		web.PathPrefixSkipper("/upload", "/app", "/api/builds")))
}

func newServer(db *sql.DB, blobs *blobStore, password, apiKey string, cookieSecure bool, publicURL string) *Server {
	return &Server{
		db:    db,
		blobs: blobs,
		auth: &web.Auth{
			DB:           db,
			Password:     password,
			APIKey:       apiKey,
			CookieSecure: cookieSecure,
		},
		publicURL: strings.TrimRight(publicURL, "/"),
		limiter:   web.NewFailLimiter(20, time.Minute),
	}
}

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		"relAge":   web.RelAge,
		"sizeText": web.HumanSize,
		"markdown": renderMarkdown,
	}
	tmpl, err := web.ParseTemplates(assets, funcs)
	if err != nil {
		return err
	}
	s.rd = &web.Renderer{Templates: tmpl, AssetVer: theme.Version, Funcs: funcs,
		App: "sideload", Mark: "si",
		Nav: []web.NavItem{
			{Label: "Builds", URL: "/"},
			{Label: "Shares", URL: "/shares"},
			{Label: "Log out", URL: "/logout"},
		},
	}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Author UI — session-gated.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleIndex))
	mux.HandleFunc("POST /upload", s.auth.RequireSession(s.handleUpload))
	mux.HandleFunc("GET /app/{bundle}", s.auth.RequireSession(s.handleApp))
	mux.HandleFunc("GET /app/{bundle}/edit", s.auth.RequireSession(s.handleAppEdit))
	mux.HandleFunc("POST /app/{bundle}/meta", s.auth.RequireSession(s.handleAppMetaSave))
	mux.HandleFunc("POST /app/{bundle}/screenshots", s.auth.RequireSession(s.handleScreenshotUpload))
	mux.HandleFunc("POST /app/{bundle}/screenshots/{sid}/caption", s.auth.RequireSession(s.handleScreenshotCaption))
	mux.HandleFunc("POST /app/{bundle}/screenshots/{sid}/move", s.auth.RequireSession(s.handleScreenshotMove))
	mux.HandleFunc("POST /app/{bundle}/screenshots/{sid}/delete", s.auth.RequireSession(s.handleScreenshotDelete))
	mux.HandleFunc("GET /app/{bundle}/devices", s.auth.RequireSession(s.handleDevices))
	mux.HandleFunc("POST /app/{bundle}/devices", s.auth.RequireSession(s.handleDeviceAdd))
	mux.HandleFunc("GET /app/{bundle}/devices.txt", s.auth.RequireSession(s.handleDevicesExport))
	mux.HandleFunc("POST /app/{bundle}/devices/import", s.auth.RequireSession(s.handleDeviceImport))
	mux.HandleFunc("POST /app/{bundle}/devices/{did}/delete", s.auth.RequireSession(s.handleDeviceDelete))
	mux.HandleFunc("POST /app/{bundle}/register/enable", s.auth.RequireSession(s.handleRegEnable))
	mux.HandleFunc("POST /app/{bundle}/register/disable", s.auth.RequireSession(s.handleRegDisable))
	mux.HandleFunc("POST /app/{bundle}/delete", s.auth.RequireSession(s.handleAppDelete))
	mux.HandleFunc("GET /b/{id}", s.auth.RequireSession(s.handleBuild))
	mux.HandleFunc("POST /b/{id}/share", s.auth.RequireSession(s.handleShareCreate))
	mux.HandleFunc("POST /b/{id}/notes", s.auth.RequireSession(s.handleBuildNotes))
	mux.HandleFunc("POST /b/{id}/delete", s.auth.RequireSession(s.handleDelete))
	mux.HandleFunc("GET /shares", s.auth.RequireSession(s.handleShares))
	mux.HandleFunc("POST /shares/{token}/revoke", s.auth.RequireSession(s.handleShareRevoke))

	// Login.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// Install session — token-gated, NO cookie (the iOS install daemon fetches
	// these). Literal final segments outrank one another cleanly under {token}.
	mux.HandleFunc("GET /i/{token}/manifest.plist", s.handleManifest)
	mux.HandleFunc("GET /i/{token}/app.ipa", s.handleIPA)
	mux.HandleFunc("GET /i/{token}/display.png", s.handleIcon(57))
	mux.HandleFunc("GET /i/{token}/full.png", s.handleIcon(512))

	// Public share landing + screenshot images (content-addressed, no token —
	// they appear on the public share page).
	mux.HandleFunc("GET /s/{token}", s.handleShareLanding)
	mux.HandleFunc("GET /shots/{sid}", s.handleScreenshot)

	// Public device registration — opt-in per app, reached by its token. The
	// .mobileconfig asks iOS to POST its UDID to the capture callback.
	mux.HandleFunc("GET /register/{token}", s.handleRegisterLanding)
	mux.HandleFunc("GET /register/{token}/enroll.mobileconfig", s.handleEnrollProfile)
	mux.HandleFunc("POST /register/{token}/capture", s.handleEnrollCapture)
	mux.HandleFunc("POST /register/{token}/submit", s.handleRegisterSubmit)
	mux.HandleFunc("GET /register/{token}/done", s.handleRegisterDone)

	// Agent API — X-API-Key.
	mux.HandleFunc("POST /api/builds", s.auth.RequireAPIKey(s.handleAPIUpload))
	mux.HandleFunc("GET /api/builds", s.auth.RequireAPIKey(s.handleAPIList))
	mux.HandleFunc("DELETE /api/builds/{id}", s.auth.RequireAPIKey(s.handleAPIDelete))
	mux.HandleFunc("POST /api/builds/{id}/share", s.auth.RequireAPIKey(s.handleAPIShare))
	mux.HandleFunc("DELETE /api/apps/{bundle}", s.auth.RequireAPIKey(s.handleAPIAppDelete))

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /static/fonts.css", theme.FontsHandler())
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// Gzip self-skips octet-stream and Range, so wrapping the whole mux leaves
	// .ipa byte serving untouched while compressing HTML/JSON. Logging sits
	// outside for the final status; pulse innermost so timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(s.pulse.Wrap(mux))),
		"GET", "POST", "DELETE", "OPTIONS")
}

// sweepLoop prunes dead share tokens hourly (and once at startup).
func (s *Server) sweepLoop() {
	for {
		if n, err := pruneTokens(s.db); err != nil {
			slog.Warn("token sweep failed", "err", err)
		} else if n > 0 {
			slog.Info("token sweep", "pruned", n)
		}
		time.Sleep(time.Hour)
	}
}

// ── validation ───────────────────────────────────────────────────────────────

// idPattern is the short content-address shape: 16 base32 chars.
var idPattern = regexp.MustCompile(`^[a-z2-7]{16}$`)

// tokenPattern is the install-token shape: 26 base32 chars (auth.NewSessionToken).
var tokenPattern = regexp.MustCompile(`^[A-Z2-7]{26}$`)

func validID(id string) bool { return idPattern.MatchString(id) }
