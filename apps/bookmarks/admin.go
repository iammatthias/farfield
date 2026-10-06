package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/iammatthias/farfield/lib/web"
)

// The private admin API (web.PrivateAPI): what the session console sees —
// private bookmarks and admin notes included — for a native client on the
// tailnet. The public API strips both and stays exactly as it was.

func (s *Server) mountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/admin/bookmarks", s.auth.PrivateAPI(s.handleAdminList))
	mux.HandleFunc("GET /api/admin/bookmarks/{id}", s.auth.PrivateAPI(s.handleAdminGet))
	mux.HandleFunc("POST /api/admin/bookmarks/{id}/refresh", s.auth.PrivateAPI(s.handleAdminRefresh))
	mux.HandleFunc(web.AdminPrefix, s.auth.PrivateAPI(web.AdminNotFound))
}

func (s *Server) handleAdminList(w http.ResponseWriter, r *http.Request) {
	bs, err := listAdminBookmarks(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list bookmarks")
		return
	}
	web.WriteJSONValidated(w, r, map[string]any{"bookmarks": bs})
}

func (s *Server) handleAdminGet(w http.ResponseWriter, r *http.Request) {
	b, err := getBookmark(s.db, r.PathValue("id"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read bookmark")
		return
	}
	if b == nil {
		web.WriteError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	web.WriteRecord(w, r, b.CID, b)
}

// handleAdminRefresh re-fetches a bookmark's page metadata while the caller
// waits — the session form's refetch, but reporting a failed fetch as a 502
// instead of saving through it. The fetch and the merge are the ones every
// other path uses (fetchMetadata, then updateBookmarkMetadata's re-read and
// fetched-fields-only write), so an edit racing the fetch survives it.
// fetchMetadata bounds the request to fetchTimeout; the context here only
// stops the work early if the client goes away.
func (s *Server) handleAdminRefresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := getBookmark(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read bookmark")
		return
	}
	if b == nil {
		web.WriteError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), fetchTimeout)
	defer cancel()
	meta, err := fetchMetadata(ctx, s.http, b.URL)
	if err != nil {
		slog.Warn("admin refresh: metadata fetch failed", "id", id, "url", b.URL, "err", err)
		web.WriteError(w, http.StatusBadGateway, "metadata fetch failed: "+err.Error())
		return
	}
	if err := updateBookmarkMetadata(s.db, id, meta); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not store fetched metadata")
		return
	}
	b, err = getBookmark(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read bookmark")
		return
	}
	if b == nil {
		web.WriteError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	web.WriteSaved(w, http.StatusOK, b.CID, b)
}
