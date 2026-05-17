package main

import (
	"net/http"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
)

// requireSession guards the HTML admin UI. An invalid or absent session
// redirects to the login page.
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
