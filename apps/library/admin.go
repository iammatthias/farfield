package main

import (
	"net/http"

	"github.com/iammatthias/farfield/lib/web"
)

// The private admin API (web.PrivateAPI): the catalog as JSON for a native
// client on the tailnet. Library has no read key, so this takes the full
// LIBRARY_API_KEY or a write-scoped admin-issued key — never the narrower
// upload key, which by design can add books but not read the library.

func (s *Server) mountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/books", s.auth.PrivateAPI(s.handleAdminBooks))
	mux.HandleFunc(web.AdminPrefix, s.auth.PrivateAPI(web.AdminNotFound))
}

// handleAdminBooks lists every book, newest first, with the named collections
// and their counts. Uncategorised books have no collection entry of their own
// — an empty name is not a folder — so their count rides alongside as
// "uncategorized", the same split the admin index shows.
func (s *Server) handleAdminBooks(w http.ResponseWriter, r *http.Request) {
	books, err := listBooks(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list books")
		return
	}
	named, uncategorized, err := collectionStats(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list collections")
		return
	}
	if books == nil {
		books = []Book{}
	}
	if named == nil {
		named = []CollectionStat{}
	}
	web.WriteJSONValidated(w, r, map[string]any{
		"books": books, "collections": named, "uncategorized": uncategorized,
	})
}
