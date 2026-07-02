package web

import (
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
)

// KeyChecker resolves an admin-issued token for an app to its scope —
// keys.ScopeRead / ScopeUpload / ScopeWrite. lib/keys implements it; the
// interface lives here so lib/web never depends on the key store.
type KeyChecker interface {
	Check(token, app string) (scope string, ok bool)
}

// Auth bundles the credentials and session storage an app's gated routes
// share. Zero-value fields fail closed: an empty Password rejects every
// login, an empty APIKey refuses every API write.
//
// ReadKey is the optional read-only bearer token. It is the one deliberately
// fail-open field: when empty, RequireReadKey leaves read endpoints public
// (their pre-token behavior), so a read token is opt-in per deployment.
//
// Keys + App optionally layer admin-issued keys (the keys app, lib/keys) on
// top of the env keys: a presented token that resolves for App is honored by
// the same gates, write scope where the APIKey is accepted and read-or-write
// scope where the ReadKey is. Env keys keep working unchanged. Assign Keys
// only from a successfully opened store — a typed-nil in the interface would
// read as configured.
type Auth struct {
	DB           *sql.DB
	Password     string
	APIKey       string
	ReadKey      string
	CookieSecure bool
	Keys         KeyChecker
	App          string
}

// keyScope resolves the request's bearer token against the admin-issued key
// store, when one is attached.
func (a *Auth) keyScope(r *http.Request) (string, bool) {
	if a.Keys == nil {
		return "", false
	}
	return a.Keys.Check(APIKeyFrom(r), a.App)
}

// RequireSession guards the HTML admin UI. An invalid or absent session
// redirects to the login page.
func (a *Auth) RequireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token, ok := auth.Session(r); ok {
			if valid, err := store.ValidSession(a.DB, token); err == nil && valid {
				next(w, r)
				return
			}
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// RequireAPIKey guards the JSON write endpoints. A missing or wrong key
// yields a 401. When neither an env key nor a key store is configured,
// writes are refused outright.
func (a *Auth) RequireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.APIKey == "" && a.Keys == nil {
			WriteError(w, http.StatusServiceUnavailable, "no API key configured")
			return
		}
		if a.HasWriteKey(r) {
			next(w, r)
			return
		}
		WriteError(w, http.StatusUnauthorized, "missing or invalid API key")
	}
}

// RequireReadKey guards read endpoints with the read-only bearer token,
// presented as "Authorization: Bearer <key>" or X-API-Key. The write APIKey is
// also accepted, since write access implies read access. When no ReadKey is
// configured the gate is open — set one (e.g. CONTENT_READ_KEY) to require a
// token for reads.
func (a *Auth) RequireReadKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.ReadKey == "" {
			next(w, r)
			return
		}
		if a.HasReadKey(r) {
			next(w, r)
			return
		}
		WriteError(w, http.StatusUnauthorized, "missing or invalid read token")
	}
}

// HasWriteKey reports whether the request carries a valid write credential —
// the env APIKey or an admin-issued write-scoped key. That is the privileged
// tier that unlocks writes and, for read endpoints, drafts. It is false when
// neither source is configured.
func (a *Auth) HasWriteKey(r *http.Request) bool {
	if a.APIKey != "" && auth.VerifyAPIKey(APIKeyFrom(r), a.APIKey) {
		return true
	}
	scope, ok := a.keyScope(r)
	return ok && scope == "write"
}

// HasReadKey reports whether the request presents a credential the read gate
// accepts: the read key, the write key (which implies read), or an
// admin-issued key with read or write scope. Unlike RequireReadKey, an
// unconfigured key is NOT treated as "open" here — with nothing to match, no
// request counts as privileged. It backs RequireReadKey and exempts trusted,
// keyed callers from rate limiting on otherwise-public endpoints.
func (a *Auth) HasReadKey(r *http.Request) bool {
	key := APIKeyFrom(r)
	if key == "" {
		return false
	}
	if (a.ReadKey != "" && auth.VerifyAPIKey(key, a.ReadKey)) ||
		(a.APIKey != "" && auth.VerifyAPIKey(key, a.APIKey)) {
		return true
	}
	scope, ok := a.keyScope(r)
	return ok && (scope == "read" || scope == "write")
}

// APIKeyFrom reads the API key from either an X-API-Key header or an
// "Authorization: Bearer <key>" header.
func APIKeyFrom(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return ""
}

// HandleLogin verifies the posted password, opens a one-week session, and
// redirects to the admin index. Wire it to POST /login; the GET form stays
// app-owned (it renders through the app's templates).
func (a *Auth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if a.Password == "" || !auth.VerifyPassword(r.FormValue("password"), a.Password) {
		http.Redirect(w, r, "/login?error=Invalid+password", http.StatusSeeOther)
		return
	}
	token := auth.NewSessionToken()
	if err := store.InsertSession(a.DB, token, time.Now().Add(7*24*time.Hour)); err != nil {
		slog.Error("create session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, auth.SessionCookie(token, a.CookieSecure))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// HandleLogout deletes the session and clears the cookie.
func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if token, ok := auth.Session(r); ok {
		_ = store.DeleteSession(a.DB, token)
	}
	http.SetCookie(w, auth.ClearCookie(a.CookieSecure))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
