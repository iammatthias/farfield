package main

import (
	"database/sql"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/iammatthias/farfield/lib/keys"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed templates
var assets embed.FS

// tmplFuncs: "day" shortens an RFC3339 timestamp to its date for the tables.
var tmplFuncs = template.FuncMap{
	"day": func(s string) string {
		if len(s) >= 10 {
			return s[:10]
		}
		return s
	},
}

// knownApps is the issue-form dropdown: every farfield app whose auth gates
// honor admin-issued keys, plus the wildcard. Add an app here when it gains
// keys.Attach in its run().
var knownApps = []string{
	keys.AppAny, "backup", "blobs", "bookmarks", "content", "feed",
	"library", "pulse", "qr", "scrap", "sideload", "switchboard",
}

// scopes describes each scope on the issue form, narrowest first.
var scopes = []struct{ Value, Label string }{
	{keys.ScopeRead, "read — token-gated read endpoints only"},
	{keys.ScopeUpload, "upload — library book upload/regroup only"},
	{keys.ScopeWrite, "write — full API writes (implies read)"},
}

// Server holds the running keys service.
type Server struct {
	db   *sql.DB
	ks   *keys.Store
	auth *web.Auth
	rd   *web.Renderer

	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder

	// wa is the passkey relying party; nil when WEBAUTHN_RP_ID is unset,
	// which turns every passkey route and link off.
	wa         *webauthn.WebAuthn
	ceremonies *ttlStore[ceremony]
	beginRL    *web.RateLimiter

	// grants are approved device authorizations awaiting redemption, keyed
	// by code; deviceFails throttles bad redemptions per client.
	grants      *ttlStore[grant]
	deviceFails *web.FailLimiter
}

// newServer wires a Server around an open database and key store. run and
// the tests both build through it, so neither can miss an in-memory store.
// The passkey table migrates itself here, like the key schema in keys.New.
func newServer(db *sql.DB, ks *keys.Store, a *web.Auth, rd *web.Renderer, wa *webauthn.WebAuthn) (*Server, error) {
	if _, err := db.Exec(passkeySchema); err != nil {
		return nil, err
	}
	if wa != nil {
		rd.Nav = append([]web.NavItem{{Label: "Passkeys", URL: "/passkeys"}}, rd.Nav...)
	}
	return &Server{
		db: db, ks: ks, auth: a, rd: rd, wa: wa,
		ceremonies:  newTTLStore[ceremony](ceremonyTTL, 1000),
		beginRL:     web.NewRateLimiter(30, time.Minute),
		grants:      newTTLStore[grant](grantTTL, 1000),
		deviceFails: web.NewFailLimiter(10, time.Minute),
	}, nil
}

// run wires up dependencies and serves until interrupted.
func run(host, port string) error {
	db, err := store.OpenDB(store.Env("KEYS_DB_PATH", "keys.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(store.SessionSchema); err != nil {
		return err
	}
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}
	ks, err := keys.New(db)
	if err != nil {
		return err
	}
	// Close stops the store's usage flusher; the keys app checks no tokens
	// itself, so there is nothing to flush, but the goroutine should not leak.
	// Closing the shared db twice is harmless.
	defer ks.Close()

	tmpl, err := web.ParseTemplates(assets, tmplFuncs)
	if err != nil {
		return err
	}

	s, err := newServer(db, ks,
		&web.Auth{
			DB:           db,
			Password:     store.Env("PASSWORD", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		&web.Renderer{Templates: tmpl, AssetVer: theme.Version, Funcs: tmplFuncs,
			App: "keys", Mark: "ke",
			Nav: []web.NavItem{
				{Label: "New key", URL: "/new"},
				{Label: "Log out", URL: "/logout"},
			},
		},
		passkeyConfig())
	if err != nil {
		return err
	}

	s.pulse = pulse.New(s.db, "keys")
	defer s.pulse.Close()
	return web.Serve(host, port, web.MaxBody(s.routes(), web.DefaultMaxBody))
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	s.rd.MountPalette(mux, s.auth, s.paletteItems)

	// HTML admin UI — session-gated. There is deliberately no JSON write API:
	// a credential minter should not itself be drivable by a credential.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleIndex))
	mux.HandleFunc("GET /new", s.auth.RequireSession(s.handleNewForm))
	mux.HandleFunc("POST /keys", s.auth.RequireSession(s.handleCreate))
	mux.HandleFunc("GET /keys/{id}", s.auth.RequireSession(s.handleKey))
	mux.HandleFunc("POST /keys/{id}/revoke", s.auth.RequireSession(s.handleRevoke))
	mux.HandleFunc("POST /keys/{id}/delete", s.auth.RequireSession(s.handleDelete))
	mux.HandleFunc("POST /keys/{id}/rename", s.auth.RequireSession(s.handleRename))
	mux.HandleFunc("POST /keys/{id}/expiry", s.auth.RequireSession(s.handleExpiry))
	mux.HandleFunc("POST /keys/{id}/rotate", s.auth.RequireSession(s.handleRotate))

	// Login — HandleLogin throttles failed attempts itself, in every app.
	// This one is also the fleet's login (FARFIELD_LOGIN_URL): it honors
	// ?next= and, with passkeys on, offers one above the password.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	if s.wa != nil {
		mux.HandleFunc("POST /passkey/login/begin", s.handlePasskeyLoginBegin)
		mux.HandleFunc("POST /passkey/login/finish", s.handlePasskeyLoginFinish)
		mux.HandleFunc("GET /passkeys", s.auth.RequireSession(s.handlePasskeys))
		mux.HandleFunc("POST /passkey/register/begin", s.requireFresh(s.handlePasskeyRegisterBegin))
		mux.HandleFunc("POST /passkey/register/finish", s.requireFresh(s.handlePasskeyRegisterFinish))
		mux.HandleFunc("POST /passkeys/{id}/delete", s.requireFresh(s.handlePasskeyDelete))
		mux.HandleFunc("GET /static/passkey.js", s.handlePasskeyJS)
		// approving a native app: the passkey is the proof, no session needed
		mux.HandleFunc("POST /passkey/device/begin", s.handleDeviceApproveBegin)
		mux.HandleFunc("POST /passkey/device/finish", s.handleDeviceApproveFinish)
	}

	// Native-app sign-in (device.go). /device/token is private ingress only.
	mux.HandleFunc("GET /device/authorize", s.handleDeviceAuthorize)
	mux.HandleFunc("POST /device/authorize", s.auth.RequireSession(s.handleDeviceDecide))
	mux.HandleFunc("POST /device/token", web.PrivateIngress(s.handleDeviceToken))

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /static/fonts.css", theme.FontsHandler())
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	return web.LogRequests(web.Gzip(s.pulse.Wrap(mux)))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ks, err := s.ks.List()
	if err != nil {
		s.fail(w, "list keys", err)
		return
	}
	active := 0
	for i := range ks {
		if ks[i].Active() {
			active++
		}
	}
	s.rd.Render(w, "index.html", map[string]any{
		"Keys":   keyViews(ks),
		"Total":  len(ks),
		"Active": active,
	})
}

func (s *Server) handleNewForm(w http.ResponseWriter, r *http.Request) {
	s.renderForm(w, "")
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var expires time.Time
	if v := r.FormValue("expires_days"); v != "" {
		days, err := strconv.Atoi(v)
		if err != nil || days <= 0 {
			s.renderForm(w, "Expiry must be a positive number of days (or empty for never).")
			return
		}
		expires = time.Now().AddDate(0, 0, days)
	}
	token, k, err := s.ks.Mint(
		r.FormValue("name"), r.FormValue("app"), r.FormValue("scope"), expires)
	if err != nil {
		s.renderForm(w, err.Error())
		return
	}
	// The token renders exactly once, here. Only its hash is stored, so there
	// is no page to come back to — copy it now.
	s.rd.Render(w, "created.html", map[string]any{
		"Token": token,
		"Key":   keyView(*k),
	})
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if _, err := s.ks.Revoke(r.PathValue("id")); err != nil {
		s.fail(w, "revoke key", err)
		return
	}
	// The key's own page sends from=key to land back on it; the index
	// stays on the index.
	if r.FormValue("from") == "key" {
		http.Redirect(w, r, keyURL(r.PathValue("id")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := s.ks.Delete(r.PathValue("id")); err != nil {
		s.fail(w, "delete key", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLoginForm renders the fleet login. Someone already signed in is sent
// straight on to next — unless this is a reauth, where the point is to prove
// it again. The passkey button shows only when there is a passkey to use.
//
// The forward is same-host only. A signed-in browser that arrives here with
// another app's URL as next was sent by that app, which means the app cannot
// see this session (a cookie domain that does not cover it; in dev, a console
// opened at 127.0.0.1 while this cookie lives on localhost). Forwarding would
// bounce between the two forever, so it gets a link to that app's own login.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	next := web.SafeNext(r, q.Get("next"))
	reauth := q.Get("reauth") != ""
	elsewhere := ""
	if !reauth && s.auth.SessionValid(r) {
		u, err := url.Parse(next)
		switch {
		case next == "":
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		case err == nil && (!u.IsAbs() || strings.EqualFold(u.Host, r.Host)):
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		case err == nil:
			elsewhere = u.Scheme + "://" + u.Host
		}
	}
	passkeys := false
	if s.wa != nil {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM passkeys`).Scan(&n); err == nil {
			passkeys = n > 0
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	s.rd.Render(w, "login.html", map[string]any{
		"Error":     q.Get("error"),
		"Next":      next,
		"Reauth":    reauth,
		"Passkeys":  passkeys,
		"Elsewhere": elsewhere,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ks, err := s.ks.List()
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	active := 0
	for i := range ks {
		if ks[i].Active() {
			active++
		}
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "keys",
		"ok":      true,
		"keys":    len(ks),
		"active":  active,
	})
}

func (s *Server) renderForm(w http.ResponseWriter, errMsg string) {
	s.rd.Render(w, "key_form.html", map[string]any{
		"Apps":   knownApps,
		"Scopes": scopes,
		"Error":  errMsg,
	})
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// view carries one key plus its display state for the index template.
type view struct {
	keys.Key
	Status string // Active | Expired | Revoked
}

func keyView(k keys.Key) view {
	v := view{Key: k, Status: "Active"}
	switch {
	case k.RevokedAt != "":
		v.Status = "Revoked"
	case !k.Active():
		v.Status = "Expired"
	}
	return v
}

func keyViews(ks []keys.Key) []view {
	out := make([]view, len(ks))
	for i, k := range ks {
		out[i] = keyView(k)
	}
	return out
}
