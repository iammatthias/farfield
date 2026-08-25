package main

import (
	"database/sql"
	"embed"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/capability"
	"github.com/iammatthias/farfield/lib/keys"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

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

	// caps is the fleet action layer — the same implementations the `farfield`
	// CLI and the agents' slash commands run. switchboard holds the service keys
	// so the phone never has to, which is the point of routing through it at all.
	caps *capability.Clients

	// reg is what this switchboard answers: the shared fleet commands plus the
	// few that only mean something with a message log behind them.
	reg *capability.Registry
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
	}

	// The keys are switchboard's own minted tokens rather than the apps' env
	// keys, which is why they are named here instead of read by lib/capability.
	s.caps = capability.New(capability.Config{
		FeedURL: store.Env("FEED_URL", "http://127.0.0.1:8788"),
		FeedKey: store.Env("SWITCHBOARD_FEED_KEY", ""),

		BookmarksURL: store.Env("BOOKMARKS_URL", "http://127.0.0.1:8793"),
		BookmarksKey: store.Env("SWITCHBOARD_BOOKMARKS_KEY", ""),
		BookmarkCat:  store.Env("SWITCHBOARD_BOOKMARK_CATEGORY", "unsorted"),

		ScrapURL: store.Env("SCRAP_URL", "http://127.0.0.1:8799"),
		ScrapKey: store.Env("SWITCHBOARD_SCRAP_KEY", ""),

		QRURL:       store.Env("QR_URL", "http://127.0.0.1:8794"),
		QRKey:       store.Env("SWITCHBOARD_QR_KEY", ""),
		QRPublicURL: store.Env("QR_PUBLIC_URL", "https://qr.farfield.systems"),

		PulseURL: store.Env("PULSE_URL", "http://127.0.0.1:8798"),
		PulseKey: store.Env("SWITCHBOARD_PULSE_KEY", ""),

		Targets: statusTargets(),
	})
	s.reg = s.commands()

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

// statusTargets is what /status probes.
//
// The default comes from lib/fleet rather than a list repeated here — the copy
// this replaces had drifted out of the registry once already, and a status page
// that silently stops watching a service is worse than no status page.
// SWITCHBOARD_TARGETS still overrides it wholesale for an unusual deployment.
func statusTargets() []capability.Target {
	if raw := store.Env("SWITCHBOARD_TARGETS", ""); raw != "" {
		return parseTargets(raw)
	}
	// switchboard runs on the host, not in the stack, so compose service names
	// do not resolve here — probe the address the containers actually publish
	// on. Skipping itself as well: a service reporting its own health in its
	// own roll-up is noise, and it is not a container to probe anyway.
	var out []capability.Target
	for _, t := range capability.HostTargets(store.Env("FARFIELD_BIND_IP", "127.0.0.1")) {
		if t.Name != "switchboard" {
			out = append(out, t)
		}
	}
	return out
}

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
func parseTargets(raw string) []capability.Target {
	var out []capability.Target
	for _, part := range strings.Split(raw, ",") {
		name, url, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		name, url = strings.TrimSpace(name), strings.TrimSpace(url)
		if name != "" && url != "" {
			out = append(out, capability.Target{Name: name, URL: url})
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
