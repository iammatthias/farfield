package main

import (
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
)

// requireSession guards the HTML admin UI. An invalid or absent session
// redirects to the login page.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.validSession(r) {
			next(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// requireAPIKey guards the JSON write endpoints. A missing or wrong key yields
// a 401. When no key is configured, writes are refused outright.
func (s *Server) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" {
			writeError(w, http.StatusServiceUnavailable, "no API key configured")
			return
		}
		if !auth.VerifyAPIKey(presentedKey(r), s.apiKey) {
			writeError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// requireCatalogAuth guards the OPDS catalog endpoints. It passes for a logged
// in admin session — so the browser loads covers and downloads without a Basic
// prompt — otherwise it falls back to HTTP Basic Auth, the scheme OPDS readers
// speak: any username with the password set to OPDS_API_KEY. A failure returns
// 401 with a Basic challenge so a reader knows to prompt for credentials.
func (s *Server) requireCatalogAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.validSession(r) {
			next(w, r)
			return
		}
		if s.apiKey != "" {
			if _, password, ok := r.BasicAuth(); ok && auth.VerifyAPIKey(password, s.apiKey) {
				next(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="farfield opds"`)
		writeError(w, http.StatusUnauthorized, "authentication required")
	}
}

// validSession reports whether the request carries a live admin session cookie.
func (s *Server) validSession(r *http.Request) bool {
	token, ok := auth.Session(r)
	if !ok {
		return false
	}
	valid, err := store.ValidSession(s.db, token)
	return err == nil && valid
}

// presentedKey reads the API key from either an X-API-Key header or an
// "Authorization: Bearer <key>" header.
func presentedKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return ""
}
