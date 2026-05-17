package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
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
	proxyBlobUpload(w, r, s.blobsURL, s.blobsKey)
}

// handleEmbedSeries builds a series fragment from an ordered set of blob CIDs.
// Series live in the content service, so the feed editor creates them there
// through content's API-key-gated endpoint and relays the result.
func (s *Server) handleEmbedSeries(w http.ResponseWriter, r *http.Request) {
	var req embedSeriesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.CIDs) == 0 {
		writeError(w, http.StatusBadRequest, "a series needs at least one blob")
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"title": strings.TrimSpace(req.Title),
		"body":  seriesBodyFromCIDs(req.CIDs),
	})
	creq, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(s.contentURL, "/")+"/api/series", bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build request")
		return
	}
	creq.Header.Set("X-API-Key", s.contentKey)
	creq.Header.Set("Content-Type", "application/json")
	resp, err := embedClient.Do(creq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "content service unreachable")
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
