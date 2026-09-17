package main

import (
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/web"
)

// Soft delete. An entry goes to trash, can come back, and only leaves for good
// on an explicit destroy — because a slug is a URL and unpublishing by accident
// is worse than keeping a row.

// handleTrash lists soft-deleted entries with per-row restore and
// delete-forever actions. Trashed rows older than trashRetention are purged
// at startup.
func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request) {
	trashed, err := listTrash(s.db)
	if err != nil {
		s.fail(w, "list trash", err)
		return
	}
	s.rd.Render(w, "trash.html", map[string]any{
		"Entries": trashed, "RetentionDays": int(trashRetention.Hours() / 24),
	})
}

// handleRestoreEntry returns a trashed entry to the live lists.
func (s *Server) handleRestoreEntry(w http.ResponseWriter, r *http.Request) {
	if _, err := restoreEntry(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "restore entry", err)
		return
	}
	http.Redirect(w, r, "/entries/trash", http.StatusSeeOther)
}

// handleDestroyEntry hard-deletes a trashed entry — the one genuinely
// destructive entry action left, and it only works on rows already in the
// trash (a live entry must be trashed first).
func (s *Server) handleDestroyEntry(w http.ResponseWriter, r *http.Request) {
	if _, err := hardDeleteEntry(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "destroy entry", err)
		return
	}
	http.Redirect(w, r, "/entries/trash", http.StatusSeeOther)
}

// entryFromForm reads an Entry from a posted admin form.
func entryFromForm(r *http.Request) *Entry {
	_ = r.ParseForm()
	title := strings.TrimSpace(r.FormValue("title"))
	slug := web.FirstNonEmpty(slugify(r.FormValue("slug")), slugify(title))
	return &Entry{
		Collection: r.FormValue("collection"),
		Slug:       slug,
		Title:      title,
		Excerpt:    strings.TrimSpace(r.FormValue("excerpt")),
		Body:       r.FormValue("body"),
		Tags:       splitTags(r.FormValue("tags")),
		Published:  r.FormValue("published") != "",
	}
}
