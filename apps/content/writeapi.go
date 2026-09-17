package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/iammatthias/farfield/lib/web"
)

// The API-key-gated write API — how an agent or a script publishes without a
// browser session.

func (s *Server) handleAPICreateEntry(w http.ResponseWriter, r *http.Request) {
	var e Entry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if e.Slug == "" {
		e.Slug = slugify(e.Title)
	}
	if e.Title == "" || e.Slug == "" || e.Collection == "" {
		web.WriteError(w, http.StatusBadRequest, "title, slug, and collection are required")
		return
	}
	if err := insertEntry(s.db, &e); err != nil {
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	web.WriteJSON(w, http.StatusCreated, e)
}

func (s *Server) handleAPIUpdateEntry(w http.ResponseWriter, r *http.Request) {
	current := r.PathValue("slug")
	var e Entry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if e.Slug == "" {
		e.Slug = current
	}
	if err := updateEntry(s.db, current, &e); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			web.WriteError(w, http.StatusNotFound, "entry not found")
			return
		}
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	web.WriteJSON(w, http.StatusOK, e)
}

// handleAPIDeleteEntry trashes an entry, like the admin delete — soft, so no
// API action is destructive either. The response shape is unchanged: to the
// caller the entry is deleted (and reads as gone); restore and purge live in
// the admin trash page.
func (s *Server) handleAPIDeleteEntry(w http.ResponseWriter, r *http.Request) {
	existed, err := deleteEntry(s.db, r.PathValue("slug"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete entry")
		return
	}
	if !existed {
		web.WriteError(w, http.StatusNotFound, "entry not found")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("slug")})
}
