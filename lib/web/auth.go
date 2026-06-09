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

// Auth bundles the credentials and session storage an app's gated routes
// share. Zero-value fields fail closed: an empty Password rejects every
// login, an empty APIKey refuses every API write.
type Auth struct {
	DB           *sql.DB
	Password     string
	APIKey       string
	CookieSecure bool
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
// yields a 401. When no key is configured, writes are refused outright.
func (a *Auth) RequireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.APIKey == "" {
			WriteError(w, http.StatusServiceUnavailable, "no API key configured")
			return
		}
		if !auth.VerifyAPIKey(APIKeyFrom(r), a.APIKey) {
			WriteError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next(w, r)
	}
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
