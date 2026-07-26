package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/web"
)

// embedClient calls the blobs and content services on behalf of the editor.
// The timeout is generous because blob uploads carry image bytes.
var embedClient = &http.Client{Timeout: 60 * time.Second}

// embedSeriesRequest is what the editor's "build new series" flow posts: a
// title and the blob CIDs that make up the gallery, in display order.
type embedSeriesRequest struct {
	Title string   `json:"title"`
	CIDs  []string `json:"cids"`
}

// handleEmbedBlob proxies a browser file upload to the blobs service so the
// blobs API key never reaches the page.
func (s *Server) handleEmbedBlob(w http.ResponseWriter, r *http.Request) {
	web.ProxyUpload(w, r, s.blobsURL, s.blobsKey, web.MaxEmbedUpload)
}

// handleEmbedBlobsList proxies the editor's paginated blob-gallery read to the
// blobs service with the server-side key — the blobs index is token-gated, so
// the browser cannot read it directly.
func (s *Server) handleEmbedBlobsList(w http.ResponseWriter, r *http.Request) {
	web.ProxyGet(w, r, strings.TrimRight(s.blobsURL, "/")+"/blobs", s.blobsKey)
}

// handleEmbedSeriesList proxies the editor's series picker to content, where
// series live, with the server-side content key.
func (s *Server) handleEmbedSeriesList(w http.ResponseWriter, r *http.Request) {
	web.ProxyGet(w, r, strings.TrimRight(s.contentURL, "/")+"/api/series", s.contentKey)
}

// handleEmbedSeries builds a series fragment from an ordered set of blob CIDs.
// Series live in the content service, so the feed editor creates them there
// through content's API-key-gated endpoint and relays the result.
func (s *Server) handleEmbedSeries(w http.ResponseWriter, r *http.Request) {
	var req embedSeriesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.CIDs) == 0 {
		web.WriteError(w, http.StatusBadRequest, "a series needs at least one blob")
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"title": strings.TrimSpace(req.Title),
		"body":  seriesBodyFromCIDs(req.CIDs),
	})
	creq, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(s.contentURL, "/")+"/api/series", bytes.NewReader(payload))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not build request")
		return
	}
	creq.Header.Set("X-API-Key", s.contentKey)
	creq.Header.Set("Content-Type", "application/json")
	resp, err := embedClient.Do(creq)
	if err != nil {
		web.WriteError(w, http.StatusBadGateway, "content service unreachable")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// seriesBodyFromCIDs renders an ordered set of blob CIDs as a series fragment
// body — one blob:// image per line, the shape the website renders as a
// gallery.
func seriesBodyFromCIDs(cids []string) string {
	lines := make([]string, 0, len(cids))
	for _, c := range cids {
		if c = strings.TrimSpace(c); c != "" {
			lines = append(lines, "![](blob://"+c+")")
		}
	}
	return strings.Join(lines, "\n\n")
}
