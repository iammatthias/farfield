package main

import (
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
)

// requireSession gates the HTML admin UI. Invalid or missing sessions redirect
// to /login.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token, ok := auth.Session(r); ok {
			if valid, err := store.ValidSession(s.db, token); err == nil && valid {
				next(w, r)
				return
			}
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// requireAPIKey gates the JSON write endpoints. With no key configured, writes
// are refused outright — the write API is disabled until QR_API_KEY is set.
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

// presentedKey reads the API key from X-API-Key or Authorization: Bearer.
func presentedKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return ""
}
