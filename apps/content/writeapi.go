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
//
// Writes that target an existing entry or series honor If-Match: the client
// sends back the ETag its GET returned and the write lands only if nobody
// saved in between — checked up front, and guarded again on the statement
// itself so the check and the write cannot be split by another save. Without
// the header they behave exactly as they always have.

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
	web.WriteSaved(w, http.StatusCreated, entryETag(&e), e)
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
	ifVer, ok := s.ifMatchEntry(w, r, current)
	if !ok {
		return
	}
	if err := updateEntryIf(s.db, current, &e, ifVer); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.entryWriteMissed(w, current, ifVer)
			return
		}
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	web.WriteSaved(w, http.StatusOK, entryETag(&e), e)
}

// handleAPIDeleteEntry trashes an entry, like the admin delete — soft, so no
// API action is destructive either. The response shape is unchanged: to the
// caller the entry is deleted (and reads as gone); restore and purge live in
// the admin trash page.
func (s *Server) handleAPIDeleteEntry(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	ifVer, ok := s.ifMatchEntry(w, r, slug)
	if !ok {
		return
	}
	existed, err := deleteEntryIf(s.db, slug, ifVer)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete entry")
		return
	}
	if !existed {
		s.entryWriteMissed(w, slug, ifVer)
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": slug})
}

// ifMatchEntry runs a write's If-Match check against the stored entry and
// returns the version to guard the write on (zero when the request carries no
// precondition). ok is false once a response is written: the 412, or a 404
// for an entry that does not exist. The tag compared is the single-entry
// GET's (entryETag), drafts included — the write key that reaches this route
// is the same key that reads drafts there.
func (s *Server) ifMatchEntry(w http.ResponseWriter, r *http.Request, slug string) (entryVersion, bool) {
	if r.Header.Get("If-Match") == "" {
		return entryVersion{}, true
	}
	cur, err := getEntry(s.db, slug)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read entry")
		return entryVersion{}, false
	}
	if cur == nil {
		web.WriteError(w, http.StatusNotFound, "entry not found")
		return entryVersion{}, false
	}
	switch web.CheckIfMatch(w, r, entryETag(cur), cur) {
	case web.Failed:
		return entryVersion{}, false
	case web.Matched:
		return versionOf(cur), true
	}
	return entryVersion{}, true
}

// entryWriteMissed answers a write whose statement matched no row. An
// unconditional write can only have missed a missing entry. A guarded one
// may instead have lost the race the If-Match check cannot see — another
// save landed between the check and the write — which is the 412.
func (s *Server) entryWriteMissed(w http.ResponseWriter, slug string, ifVer entryVersion) {
	if ifVer == (entryVersion{}) {
		web.WriteError(w, http.StatusNotFound, "entry not found")
		return
	}
	cur, err := getEntry(s.db, slug)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read entry")
		return
	}
	if cur == nil {
		web.WriteError(w, http.StatusNotFound, "entry not found")
		return
	}
	web.WritePreconditionFailed(w, entryETag(cur), cur)
}
