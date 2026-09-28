package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// embedClient calls the blobs service on behalf of the editor. The timeout is
// generous because uploads carry image bytes.
var embedClient = &http.Client{Timeout: 60 * time.Second}

// embedSeriesRequest is what the editor's "build new series" flow posts: a
// title and the blob CIDs that make up the gallery, in display order.
type embedSeriesRequest struct {
	Title string   `json:"title"`
	CIDs  []string `json:"cids"`
}

// handleEmbedBlob proxies a browser file upload to the blobs service so the
// blobs API key never reaches the page. The response is the new blob's
// metadata JSON, including its CID.
func (s *Server) handleEmbedBlob(w http.ResponseWriter, r *http.Request) {
	web.ProxyUpload(w, r, s.blobsURL, s.blobsKey, web.MaxEmbedUpload)
}

// handleEmbedBlobsList proxies the editor's paginated blob-gallery read to the
// blobs service with the server-side key. The blobs index is token-gated now,
// so the browser cannot read it directly; this session-gated proxy keeps the
// key off the page.
func (s *Server) handleEmbedBlobsList(w http.ResponseWriter, r *http.Request) {
	web.ProxyGet(w, r, strings.TrimRight(s.blobsURL, "/")+"/blobs", s.blobsKey)
}

// handleEmbedSeriesList returns the series list for the editor's series picker.
// Content hosts series, so it reads its own table directly rather than calling
// its own now-gated API.
func (s *Server) handleEmbedSeriesList(w http.ResponseWriter, r *http.Request) {
	series, err := listSeries(s.db)
	if err != nil {
		s.fail(w, "list series", err)
		return
	}
	if series == nil {
		series = []Series{}
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"series": series})
}

// handleEmbedSeries builds a series fragment from an ordered set of blob CIDs
// and returns it, so the editor can embed series://<slug>.
func (s *Server) handleEmbedSeries(w http.ResponseWriter, r *http.Request) {
	var req embedSeriesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.CIDs) == 0 {
		web.WriteError(w, http.StatusBadRequest, "a series needs at least one blob")
		return
	}
	now := store.NowRFC3339()
	se := &Series{
		Slug:      uniqueSlug(s.db, slugify(req.Title)),
		Title:     strings.TrimSpace(req.Title),
		Body:      seriesBodyFromCIDs(req.CIDs),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := upsertSeries(s.db, se); err != nil {
		s.fail(w, "create series", err)
		return
	}
	web.WriteJSON(w, http.StatusCreated, se)
}

// handleAPICreateSeries creates a series fragment from a posted JSON body. It
// is API-key-gated and lets other apps (the feed editor) create series here,
// since series live in content. A slug is always assigned, never rejected.
func (s *Server) handleAPICreateSeries(w http.ResponseWriter, r *http.Request) {
	var se Series
	if err := json.NewDecoder(r.Body).Decode(&se); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	se.Slug = uniqueSlug(s.db, web.FirstNonEmpty(slugify(se.Slug), slugify(se.Title)))
	se.Title = strings.TrimSpace(se.Title)
	now := store.NowRFC3339()
	se.CreatedAt, se.UpdatedAt = now, now
	if err := upsertSeries(s.db, &se); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not create series")
		return
	}
	web.WriteJSON(w, http.StatusCreated, se)
}

// handleAPIUpdateSeries replaces an existing series fragment's title and body,
// keeping its slug — the entries that embed it by series:// never change.
// It is the scripted twin of the admin form's save: without it a fragment
// could only be edited through a browser session, so a bulk rewrite of media
// refs (swapping one blob for another across every gallery) had no path.
func (s *Server) handleAPIUpdateSeries(w http.ResponseWriter, r *http.Request) {
	se, err := getSeries(s.db, r.PathValue("slug"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read series")
		return
	}
	if se == nil {
		web.WriteError(w, http.StatusNotFound, "series not found")
		return
	}
	var in struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.Body == nil {
		web.WriteError(w, http.StatusBadRequest, "body is required")
		return
	}
	if in.Title != nil {
		se.Title = strings.TrimSpace(*in.Title)
	}
	se.Body = *in.Body
	se.UpdatedAt = store.NowRFC3339()
	if err := upsertSeries(s.db, se); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not update series")
		return
	}
	web.WriteJSON(w, http.StatusOK, se)
}

// uniqueSlug returns a slug based on candidate that no series uses yet — the
// candidate itself when free, a random key when empty, else a suffixed key.
func uniqueSlug(db *sql.DB, candidate string) string {
	if candidate == "" {
		return store.ShortID()
	}
	if existing, _ := getSeries(db, candidate); existing == nil {
		return candidate
	}
	return candidate + "-" + store.ShortID()
}

// seriesBodyFromCIDs renders an ordered set of blob CIDs as a series fragment
// body — one blob:// image per line, the shape the website resolves and
// renders as a gallery.
func seriesBodyFromCIDs(cids []string) string {
	lines := make([]string, 0, len(cids))
	for _, c := range cids {
		if c = strings.TrimSpace(c); c != "" {
			lines = append(lines, "![](blob://"+c+")")
		}
	}
	return strings.Join(lines, "\n\n")
}
