package main

import (
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
)

// requireSession guards the HTML admin UI. An invalid or absent session
// redirects to the admin login page.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token, ok := auth.Session(r); ok {
			if valid, err := store.ValidSession(s.db, token); err == nil && valid {
				next(w, r)
				return
			}
		}
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	}
}

// requireAPIKey guards the JSON write endpoints. A missing or wrong key yields
// a 401. When no key is configured, writes are refused outright — the optional
// write API is disabled until BOOKMARKS_API_KEY is set.
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
