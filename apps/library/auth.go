package main

import (
	"net/http"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/keys"
	"github.com/iammatthias/farfield/lib/web"
)

// requireCatalogAuth guards the OPDS catalog endpoints. It passes for a logged
// in admin session — so the browser loads covers and downloads without a Basic
// prompt — otherwise it falls back to HTTP Basic Auth, the scheme OPDS readers
// speak: any username with the password set to a catalog credential. A failure
// returns 401 with a Basic challenge so a reader knows to prompt for
// credentials.
func (s *Server) requireCatalogAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth.SessionValid(r) || s.catalogCredential(r) {
			next(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="farfield library"`)
		web.WriteError(w, http.StatusUnauthorized, "authentication required")
	}
}

// catalogCredential reports whether the request's Basic password is a catalog
// credential: the full LIBRARY_API_KEY, or an admin-issued key with read or
// write scope — so an e-reader can hold a revocable key instead of the master
// one. Upload scope stays out, matching requireUploadKey's contract: that key
// adds books but must not read the library.
func (s *Server) catalogCredential(r *http.Request) bool {
	_, password, ok := r.BasicAuth()
	if !ok || password == "" {
		return false
	}
	if s.auth.APIKey != "" && auth.VerifyAPIKey(password, s.auth.APIKey) {
		return true
	}
	if s.auth.Keys != nil {
		if scope, ok := s.auth.Keys.Check(password, s.auth.App); ok &&
			(scope == keys.ScopeRead || scope == keys.ScopeWrite) {
			return true
		}
	}
	return false
}

// requireUploadKey guards the book-upload and regroup endpoints. It accepts
// the full LIBRARY_API_KEY, the narrower LIBRARY_UPLOAD_KEY (the "intern"
// key), or an admin-issued key with upload or write scope — presented as
// X-API-Key or Authorization: Bearer. Upload credentials are honoured only
// here — never by delete or the catalog — so they can add and organize books
// without the power to remove them or read the library. When nothing broader
// is configured, only the full key passes.
func (s *Server) requireUploadKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := web.APIKeyFrom(r)
		if key != "" &&
			((s.auth.APIKey != "" && auth.VerifyAPIKey(key, s.auth.APIKey)) ||
				(s.uploadKey != "" && auth.VerifyAPIKey(key, s.uploadKey))) {
			next(w, r)
			return
		}
		if s.auth.Keys != nil {
			if scope, ok := s.auth.Keys.Check(key, s.auth.App); ok &&
				(scope == "upload" || scope == "write") {
				next(w, r)
				return
			}
		}
		web.WriteError(w, http.StatusUnauthorized, "missing or invalid API key")
	}
}
