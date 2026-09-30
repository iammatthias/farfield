package main

import (
	"context"
	"database/sql"
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

// wordCount is the edit page's initial word count; the editor recounts live.
func wordCount(body string) int {
	return len(strings.Fields(body))
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
