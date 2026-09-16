package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Form rendering and the small helpers the handlers share: re-rendering a form
// with its error, building the public site URL, and shaping revisions for the
// template.

func (s *Server) renderCollectionForm(w http.ResponseWriter, c *Collection, isNew bool, action, errMsg string) {
	s.rd.Render(w, "collection_form.html", map[string]any{
		"Collection": c, "IsNew": isNew, "Action": action, "Error": errMsg,
	})
}

func (s *Server) renderEntryForm(w http.ResponseWriter, r *http.Request, e *Entry, collections []Collection, isNew bool, action, errMsg string) {
	s.rd.Render(w, "entry_form.html", map[string]any{
		"Entry": e, "Collections": collections, "IsNew": isNew,
		"Action": action, "Error": errMsg, "TagsText": strings.Join(e.Tags, ", "),
		"BlobsPublic": s.blobsPublic, "ContentPublic": s.contentPublic,
		"BodyHTML": s.bodyHTML(r, e.Body), "Words": wordCount(e.Body),
		"Revisions": s.revisionViews(e), "SiteURL": s.siteURL(e),
	})
}

// siteURL is the entry's public page — SITE_URL_TEMPLATE with {collection}
// and {slug} filled in. Empty when the template is unset (feature off) or the
// entry is unpublished (a draft has no public page yet), so the template just
// checks .SiteURL.
func (s *Server) siteURL(e *Entry) string {
	if s.siteURLTmpl == "" || !e.Published {
		return ""
	}
	return strings.NewReplacer(
		"{collection}", e.Collection,
		"{slug}", e.Slug,
	).Replace(s.siteURLTmpl)
}

// revisionViews shapes an entry's history for the edit page rail.
func (s *Server) revisionViews(e *Entry) []map[string]any {
	if e.ID == 0 {
		return nil
	}
	revs, err := listRevisions(s.db, e.ID, 10)
	if err != nil {
		slog.Warn("list revisions", "slug", e.Slug, "err", err)
		return nil
	}
	views := make([]map[string]any, 0, len(revs))
	words := make([]int, len(revs))
	for i, rv := range revs {
		words[i] = revisionWords(rv.Body)
	}
	for i, rv := range revs {
		when := rv.SavedAt
		if t, err := time.Parse(time.RFC3339, rv.SavedAt); err == nil {
			when = t.Local().Format("Jan 2 15:04")
		}
		// The delta against the next-older revision is what this save DID —
		// the number worth scanning for. The oldest visible row has nothing
		// to compare against, so it shows only its size.
		delta := ""
		if i+1 < len(revs) {
			switch d := words[i] - words[i+1]; {
			case d > 0:
				delta = fmt.Sprintf("+%d", d)
			case d < 0:
				delta = fmt.Sprintf("−%d", -d)
			default:
				delta = "±0"
			}
		}
		views = append(views, map[string]any{
			"ID": rv.ID, "When": when,
			"Words":   words[i],
			"Delta":   delta,
			"Current": rv.CID == e.CID,
		})
	}
	return views
}

// reRenderEntryForm re-shows the entry form after a failed submit.
func (s *Server) reRenderEntryForm(w http.ResponseWriter, r *http.Request, e *Entry, isNew bool, action, errMsg string) {
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	s.renderEntryForm(w, r, e, collections, isNew, action, errMsg)
}

// fail logs an internal error and returns a 500.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
