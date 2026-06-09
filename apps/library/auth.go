package main

import (
	"net/http"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// requireCatalogAuth guards the OPDS catalog endpoints. It passes for a logged
// in admin session — so the browser loads covers and downloads without a Basic
// prompt — otherwise it falls back to HTTP Basic Auth, the scheme OPDS readers
// speak: any username with the password set to LIBRARY_API_KEY. A failure returns
// 401 with a Basic challenge so a reader knows to prompt for credentials.
func (s *Server) requireCatalogAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.validSession(r) {
			next(w, r)
			return
		}
		if s.auth.APIKey != "" {
			if _, password, ok := r.BasicAuth(); ok && auth.VerifyAPIKey(password, s.auth.APIKey) {
				next(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="farfield library"`)
		web.WriteError(w, http.StatusUnauthorized, "authentication required")
	}
}

// validSession reports whether the request carries a live admin session cookie.
func (s *Server) validSession(r *http.Request) bool {
	token, ok := auth.Session(r)
	if !ok {
		return false
	}
	valid, err := store.ValidSession(s.auth.DB, token)
	return err == nil && valid
}
