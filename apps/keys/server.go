package main

import (
	"database/sql"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"

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
	keys.AppAny, "blobs", "bookmarks", "content", "feed",
	"library", "qr", "scrap", "sideload",
}

// scopes describes each scope on the issue form, narrowest first.
var scopes = []struct{ Value, Label string }{
	{keys.ScopeRead, "read — token-gated read endpoints only"},
	{keys.ScopeUpload, "upload — library book upload/regroup only"},
	{keys.ScopeWrite, "write — full API writes (implies read)"},
}

// Server holds the running keys service.
type Server struct {
	db    *sql.DB
	ks    *keys.Store
	auth  *web.Auth
	rd    *web.Renderer
	logrl *web.FailLimiter // failed logins, per client IP

	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder
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

	tmpl, err := web.ParseTemplates(assets, tmplFuncs)
	if err != nil {
		return err
	}

	s := &Server{
		db: db,
		ks: ks,
		auth: &web.Auth{
			DB:           db,
			Password:     store.Env("PASSWORD", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd:    &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
		logrl: web.NewFailLimiter(5, time.Minute),
	}

	s.pulse = pulse.New(s.db, "keys")
	defer s.pulse.Close()
	return web.Serve(host, port, s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// HTML admin UI — session-gated. There is deliberately no JSON write API:
	// a credential minter should not itself be drivable by a credential.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleIndex))
	mux.HandleFunc("GET /new", s.auth.RequireSession(s.handleNewForm))
	mux.HandleFunc("POST /keys", s.auth.RequireSession(s.handleCreate))
	mux.HandleFunc("POST /keys/{id}/revoke", s.auth.RequireSession(s.handleRevoke))
	mux.HandleFunc("POST /keys/{id}/delete", s.auth.RequireSession(s.handleDelete))

	// Login — failure-limited: this app mints credentials, so its own front
	// door gets brute-force protection.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", web.FailLimit(s.logrl, s.auth.HandleLogin))
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	mux.HandleFunc("GET /status", s.handleStatus)
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
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := s.ks.Delete(r.PathValue("id")); err != nil {
		s.fail(w, "delete key", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
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
