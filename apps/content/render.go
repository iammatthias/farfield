package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/iammatthias/farfield/lib/markdown"
	"github.com/iammatthias/farfield/lib/web"
)

// newRenderer builds the admin-preview markdown renderer. Entries are
// long-form, so single newlines stay soft (standard paragraph semantics).
// series:// embeds resolve against this app's own series table; a series
// body renders through a plain renderer — series cannot nest.
func newRenderer(db *sql.DB, blobsURL, blobsPublic string) *markdown.Renderer {
	inner := &markdown.Renderer{MetaBase: blobsURL, PublicBase: blobsPublic}
	return &markdown.Renderer{
		MetaBase:   blobsURL,
		PublicBase: blobsPublic,
		Series: func(ctx context.Context, slug string) (string, bool) {
			se, err := getSeries(db, slug)
			if err != nil || se == nil {
				return "", false
			}
			return string(inner.Render(ctx, se.Body)), true
		},
	}
}

// maxPreviewBody caps the preview endpoint's request body — far above any
// real entry, well below abuse.
const maxPreviewBody = 2 << 20

// handlePreview renders posted markdown to HTML for the editor's live
// preview. Session-gated: it renders exactly what the edit page would.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPreviewBody)).Decode(&req); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"html": string(s.md.Render(r.Context(), req.Body)),
	})
}

// handleEditdoc renders posted markdown to the constrained editable HTML the
// document editor manipulates in place. Session-gated like the preview.
func (s *Server) handleEditdoc(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPreviewBody)).Decode(&req); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"html": string(s.md.RenderEditable(r.Context(), req.Body)),
	})
}

// bodyHTML renders a stored body for the edit page's document card; an empty
// body stays empty so the template shows the placeholder.
func (s *Server) bodyHTML(r *http.Request, body string) template.HTML {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return s.md.Render(r.Context(), body)
}

// wordCount is the edit page's initial word count; the editor recounts live.
func wordCount(body string) int {
	return len(strings.Fields(body))
}

// wantsJSON reports whether the client asked for a JSON response — the
// editor's async saves do, browser form posts don't.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// staticHandler serves the app's embedded static assets. The vendored
// ternlight engine never changes for a given URL (?v=…), so it caches
// immutably; other assets revalidate.
func staticHandler() http.Handler {
	fs := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/ternlight/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fs.ServeHTTP(w, r)
	})
}

// searchSnippet trims a body for the search corpus without splitting a rune.
func searchSnippet(body string, max int) string {
	if len(body) <= max {
		return body
	}
	cut := body[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// handleSearchData returns every entry (drafts included — this is the admin)
// as a compact corpus for the entries page's client-side semantic search.
// The per-entry CID keys the client's embedding cache: content-addressed, so
// a cached vector can never go stale.
func (s *Server) handleSearchData(w http.ResponseWriter, r *http.Request) {
	entries, err := listEntriesFull(s.db, "", statusAll, 0, 0)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list entries")
		return
	}
	docs := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		docs = append(docs, map[string]any{
			"slug":       e.Slug,
			"cid":        e.CID,
			"title":      e.Title,
			"excerpt":    e.Excerpt,
			"tags":       e.Tags,
			"collection": e.Collection,
			"published":  e.Published,
			"snippet":    searchSnippet(e.Body, 500),
		})
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"entries": docs})
}
