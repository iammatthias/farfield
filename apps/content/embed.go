package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
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
	proxyBlobUpload(w, r, s.blobsURL, s.blobsKey)
}

// handleEmbedSeries builds a series fragment from an ordered set of blob CIDs
// and returns it, so the editor can embed series://<slug>.
func (s *Server) handleEmbedSeries(w http.ResponseWriter, r *http.Request) {
	var req embedSeriesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.CIDs) == 0 {
		writeError(w, http.StatusBadRequest, "a series needs at least one blob")
		return
	}
	now := nowRFC3339()
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
	writeJSON(w, http.StatusCreated, se)
}

// handleAPICreateSeries creates a series fragment from a posted JSON body. It
// is API-key-gated and lets other apps (the feed editor) create series here,
// since series live in content. A slug is always assigned, never rejected.
func (s *Server) handleAPICreateSeries(w http.ResponseWriter, r *http.Request) {
	var se Series
	if err := json.NewDecoder(r.Body).Decode(&se); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	se.Slug = uniqueSlug(s.db, firstNonEmpty(slugify(se.Slug), slugify(se.Title)))
	se.Title = strings.TrimSpace(se.Title)
	now := nowRFC3339()
	se.CreatedAt, se.UpdatedAt = now, now
	if err := upsertSeries(s.db, &se); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create series")
		return
	}
	writeJSON(w, http.StatusCreated, se)
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

// proxyBlobUpload forwards a browser multipart upload ("file") to the blobs
// service as raw bytes with the API key attached, and relays the response.
func proxyBlobUpload(w http.ResponseWriter, r *http.Request, blobsURL, apiKey string) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid upload")
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing file")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read upload")
		return
	}
	req, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(blobsURL, "/")+"/blobs", bytes.NewReader(data))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build request")
		return
	}
	req.Header.Set("X-API-Key", apiKey)
	if ct := hdr.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := embedClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "blobs service unreachable")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
