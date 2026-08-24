package main

import (
	"database/sql"
	"embed"
	"log/slog"
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

// target is one service the /status roll-up probes.
type target struct {
	Name string
	URL  string
}

// Server holds the running switchboard service.
type Server struct {
	db    *sql.DB
	auth  *web.Auth
	rd    *web.Renderer
	pulse *pulse.Recorder

	// photon is the iMessage line: inbound arrives by webhook, outbound
	// (replies, images) and attachment bytes go back through this client.
	photon *photonClient

	// webhookSecret gates the hook. Empty disables the route entirely.
	webhookSecret string

	// allow is the set of handles permitted to act, normalized. Empty allows
	// nobody — the gate fails closed.
	allow map[string]bool

	// rl bounds how fast one sender can drive the fleet.
	rl *web.RateLimiter

	// Sibling services. switchboard holds their keys so the phone never has to.
	feed      *feedClient
	bookmarks *bookmarksClient
	scrap     *scrapClient
	qr        *qrClient
	pulseSvc  svc

	targets []target
}

// inboundPerMin bounds one sender's message rate. Far above texting speed, far
// below anything that could hammer the fleet.
const inboundPerMin = 60

// logPageSize is how many messages the console shows.
const logPageSize = 100

// retention bounds the message log. It is an operational aid — the records it
// points at live in the apps that own them — so it is bounded from the start
// rather than growing for the life of the deployment.
const retention = 90 * 24 * time.Hour

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("SWITCHBOARD_DB_PATH", "switchboard.sqlite"))
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
			APIKey:       store.Env("SWITCHBOARD_API_KEY", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd: &web.Renderer{Templates: tmpl, AssetVer: theme.Version,
			App: "switchboard", Mark: "sw",
			Nav: []web.NavItem{{Label: "Log out", URL: "/logout"}},
		},
		webhookSecret: store.Env("SWITCHBOARD_WEBHOOK_SECRET", ""),
		allow:         parseAllow(store.Env("SWITCHBOARD_ALLOW", "")),
		rl:            web.NewRateLimiter(inboundPerMin, time.Minute),
		targets:       parseTargets(store.Env("SWITCHBOARD_TARGETS", defaultTargets)),
	}

	s.feed = &feedClient{svc: newSvc(
		store.Env("FEED_URL", "http://127.0.0.1:8788"), store.Env("SWITCHBOARD_FEED_KEY", ""))}
	s.bookmarks = &bookmarksClient{
		svc: newSvc(store.Env("BOOKMARKS_URL", "http://127.0.0.1:8793"),
			store.Env("SWITCHBOARD_BOOKMARKS_KEY", "")),
		DefaultCategory: store.Env("SWITCHBOARD_BOOKMARK_CATEGORY", "unsorted"),
	}
	s.scrap = &scrapClient{svc: newSvc(
		store.Env("SCRAP_URL", "http://127.0.0.1:8799"), store.Env("SWITCHBOARD_SCRAP_KEY", ""))}
	s.qr = &qrClient{
		svc:       newSvc(store.Env("QR_URL", "http://127.0.0.1:8794"), store.Env("SWITCHBOARD_QR_KEY", "")),
		PublicURL: strings.TrimRight(store.Env("QR_PUBLIC_URL", "https://qr.farfield.systems"), "/"),
	}
	s.pulseSvc = newSvc(store.Env("PULSE_URL", "http://127.0.0.1:8798"),
		store.Env("SWITCHBOARD_PULSE_KEY", ""))

	s.photon, err = newPhotonClient(
		store.Env("SPECTRUM_PROJECT_ID", ""),
		store.Env("SPECTRUM_PROJECT_SECRET", ""),
		store.Env("SPECTRUM_IMESSAGE_ADDRESS", "imessage.spectrum.photon.codes:443"),
		store.Env("SPECTRUM_CLOUD_URL", "https://spectrum.photon.codes"),
	)
	if err != nil {
		return err
	}
	defer s.photon.Close()

	// Say plainly at boot which half is missing, because either one alone looks
	// like a working service that silently does nothing.
	if s.webhookSecret == "" {
		slog.Warn("SWITCHBOARD_WEBHOOK_SECRET unset — the webhook is disabled")
	}
	if len(s.allow) == 0 {
		slog.Warn("SWITCHBOARD_ALLOW unset — every inbound message will be ignored")
	}
	if s.photon == nil {
		slog.Warn("SPECTRUM_PROJECT_ID/SECRET unset — no replies, no photo fetching")
	}

	defer keys.Attach(s.auth, "switchboard")() // admin-issued keys, when KEYS_DB_PATH is set

	go s.sweepLoop() // retention is hourly, not once at boot

	s.pulse = pulse.New(s.db, "switchboard")
	defer s.pulse.Close()
	return web.Serve(host, port, web.MaxBody(s.routes(), web.DefaultMaxBody))
}

// defaultTargets is the fleet as docker-compose names it. Overridden wholesale
// by SWITCHBOARD_TARGETS.
const defaultTargets = "content=http://content:8787,feed=http://feed:8788," +
	"blobs=http://blobs:8789,apex=http://apex:8790,backup=http://backup:8791," +
	"daily=http://daily:8792,bookmarks=http://bookmarks:8793,qr=http://qr:8794," +
	"bard=http://bard:8795,dead-presidents=http://dead-presidents:8796," +
	"library=http://library:8797,pulse=http://pulse:8798,scrap=http://scrap:8799," +
	"sideload=http://sideload:8800,keys=http://keys:8801"

// parseAllow builds the sender allowlist, normalizing each entry the same way
// an inbound handle is normalized so the two can be compared directly.
func parseAllow(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if h := normalizeHandle(part); h != "" {
			out[h] = true
		}
	}
	return out
}

// parseTargets parses a "name=url,name=url" list.
func parseTargets(raw string) []target {
	var out []target
	for _, part := range strings.Split(raw, ",") {
		name, url, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		name, url = strings.TrimSpace(name), strings.TrimSpace(url)
		if name != "" && url != "" {
			out = append(out, target{Name: name, URL: url})
		}
	}
	return out
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// The console — session-gated. It shows what arrived and what was done
	// with it, which is the only view into a service whose real UI is Messages.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleIndex))

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// The webhook. Deliberately not behind RequireAPIKey: Photon signs each
	// delivery with an HMAC, and handleWebhook verifies that before anything
	// else. It is the only unauthenticated-by-farfield route here.
	mux.HandleFunc("POST /hooks/photon", s.handleWebhook)

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	return web.CORS(web.LogRequests(web.Gzip(s.pulse.Wrap(mux))),
		"GET", "POST", "OPTIONS")
}

// sweepLoop applies the retention promise hourly, not just at boot — a window
// enforced once at startup only holds for a process that keeps restarting.
func (s *Server) sweepLoop() {
	for {
		time.Sleep(time.Hour)
		cutoff := time.Now().Add(-retention).UTC().Format(time.RFC3339)
		if err := pruneMessages(s.db, cutoff); err != nil {
			slog.Warn("could not prune messages", "err", err)
		}
		if err := store.PruneSessions(s.db); err != nil {
			slog.Warn("could not prune sessions", "err", err)
		}
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	msgs, err := listMessages(s.db, logPageSize)
	if err != nil {
		s.fail(w, "list messages", err)
		return
	}
	s.rd.Render(w, "index.html", map[string]any{
		"Messages": msgs,
		"Ready":    s.webhookSecret != "" && len(s.allow) > 0 && s.photon != nil,
		"Senders":  len(s.allow),
	})
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countMessages(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "switchboard", "ok": true, "messages": n,
		"hook": s.webhookSecret != "", "line": s.photon != nil,
	})
}

// fail logs an internal error and returns a 500.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
