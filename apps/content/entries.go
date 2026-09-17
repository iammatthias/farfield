package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/iammatthias/farfield/lib/web"
)

// Entries: the list, the editor, saving, and the revision restore. This is the
// app's centre of gravity — everything else exists to serve what happens here.

func (s *Server) handleEntries(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("collection")
	entries, err := listEntries(s.db, filter, statusAll, 0)
	if err != nil {
		s.fail(w, "list entries", err)
		return
	}
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	s.rd.Render(w, "entries.html", map[string]any{
		"Entries": entries, "Collections": collections, "Filter": filter,
	})
}

func (s *Server) handleNewEntry(w http.ResponseWriter, r *http.Request) {
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	if len(collections) == 0 {
		http.Redirect(w, r, "/collections/new", http.StatusSeeOther)
		return
	}
	s.renderEntryForm(w, r, &Entry{Published: false}, collections, true, "/entries", "")
}

func (s *Server) handleCreateEntry(w http.ResponseWriter, r *http.Request) {
	e := entryFromForm(r)
	if e.Title == "" || e.Slug == "" || e.Collection == "" {
		s.entrySaveError(w, r, e, true, "/entries", "Title and collection are required.")
		return
	}
	if err := insertEntry(s.db, e); err != nil {
		s.entrySaveError(w, r, e, true, "/entries", err.Error())
		return
	}
	s.entrySaved(w, r, e)
}

func (s *Server) handleEditEntry(w http.ResponseWriter, r *http.Request) {
	e, err := getEntry(s.db, r.PathValue("slug"))
	if err != nil {
		s.fail(w, "get entry", err)
		return
	}
	if e == nil {
		http.NotFound(w, r)
		return
	}
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	s.renderEntryForm(w, r, e, collections, false, "/entries/"+e.Slug, "")
}

// handleRestoreRevision copies a saved revision's title and body back onto
// the entry. The restore is itself a normal save, so it lands in history too
// — restoring can never destroy state.
func (s *Server) handleRestoreRevision(w http.ResponseWriter, r *http.Request) {
	e, err := getEntry(s.db, r.PathValue("slug"))
	if err != nil {
		s.fail(w, "get entry", err)
		return
	}
	revID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rev, err := getRevision(s.db, revID)
	if err != nil {
		s.fail(w, "get revision", err)
		return
	}
	if e == nil || rev == nil || rev.EntryID != e.ID {
		http.NotFound(w, r)
		return
	}
	e.Title, e.Body = rev.Title, rev.Body
	if err := updateEntry(s.db, e.Slug, e); err != nil {
		s.fail(w, "restore revision", err)
		return
	}
	http.Redirect(w, r, "/entries/"+e.Slug+"/edit", http.StatusSeeOther)
}

func (s *Server) handleUpdateEntry(w http.ResponseWriter, r *http.Request) {
	current := r.PathValue("slug")
	e := entryFromForm(r)
	if e.Title == "" || e.Slug == "" || e.Collection == "" {
		s.entrySaveError(w, r, e, false, "/entries/"+current, "Title and collection are required.")
		return
	}
	if err := updateEntry(s.db, current, e); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.entrySaveError(w, r, e, false, "/entries/"+current, err.Error())
		return
	}
	s.entrySaved(w, r, e)
}

// entrySaved answers a successful create or update: JSON with the entry's
// canonical URLs for the editor's async saves (the slug may have changed),
// a redirect to the list for a plain form post.
func (s *Server) entrySaved(w http.ResponseWriter, r *http.Request, e *Entry) {
	if web.WantsJSON(r) {
		web.WriteJSON(w, http.StatusOK, map[string]any{
			"slug":    e.Slug,
			"action":  "/entries/" + e.Slug,
			"editURL": "/entries/" + e.Slug + "/edit",
		})
		return
	}
	http.Redirect(w, r, "/entries", http.StatusSeeOther)
}

// entrySaveError answers a failed create or update: a JSON error for the
// editor's async saves, the re-rendered form for a plain post.
func (s *Server) entrySaveError(w http.ResponseWriter, r *http.Request, e *Entry, isNew bool, action, msg string) {
	if web.WantsJSON(r) {
		web.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	s.reRenderEntryForm(w, r, e, isNew, action, msg)
}

// handleDeleteEntry moves an entry to the trash. The delete is soft — with
// revisions covering edits and the trash covering deletes, no entry admin
// action is destructive anymore; "delete forever" lives on the trash page.
func (s *Server) handleDeleteEntry(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteEntry(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "delete entry", err)
		return
	}
	http.Redirect(w, r, "/entries", http.StatusSeeOther)
}
