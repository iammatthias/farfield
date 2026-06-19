package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
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
	proxyBlobUpload(w, r, s.blobsURL, s.blobsKey)
}

// handleEmbedBlobsList proxies the editor's paginated blob-gallery read to the
// blobs service with the server-side key. The blobs index is token-gated now,
// so the browser cannot read it directly; this session-gated proxy keeps the
// key off the page.
func (s *Server) handleEmbedBlobsList(w http.ResponseWriter, r *http.Request) {
	proxyGet(w, r, strings.TrimRight(s.blobsURL, "/")+"/blobs", s.blobsKey)
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

// proxyGet forwards a GET (with its query string) to an internal farfield
// service using the server-side API key, streaming the JSON response back. It
// lets a session-gated editor read a now-token-gated sibling service without
// the key ever reaching the page.
func proxyGet(w http.ResponseWriter, r *http.Request, target, apiKey string) {
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "bad upstream request")
		return
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := embedClient.Do(req)
	if err != nil {
		web.WriteError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
	se.Slug = uniqueSlug(s.db, firstNonEmpty(slugify(se.Slug), slugify(se.Title)))
	se.Title = strings.TrimSpace(se.Title)
	now := store.NowRFC3339()
	se.CreatedAt, se.UpdatedAt = now, now
	if err := upsertSeries(s.db, &se); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not create series")
		return
	}
	web.WriteJSON(w, http.StatusCreated, se)
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

// maxEmbedUpload caps a proxied upload — it matches the blobs service's own
// 100 MiB limit, so the proxy never accepts more than blobs would.
const maxEmbedUpload = 100 << 20

// proxyBlobUpload forwards a browser multipart upload ("file") to the blobs
// service as raw bytes with the API key attached, and relays the response.
// The file part streams straight through as the upstream request body — the
// upload is never buffered in memory.
func proxyBlobUpload(w http.ResponseWriter, r *http.Request, blobsURL, apiKey string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxEmbedUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid upload")
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			web.WriteError(w, http.StatusBadRequest, "missing file")
			return
		}
		if err != nil {
			web.WriteError(w, http.StatusBadRequest, "invalid upload")
			return
		}
		if part.FormName() != "file" {
			continue
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
			strings.TrimRight(blobsURL, "/")+"/blobs", part)
		if err != nil {
			web.WriteError(w, http.StatusInternalServerError, "could not build request")
			return
		}
		req.Header.Set("X-API-Key", apiKey)
		if ct := part.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := embedClient.Do(req)
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				web.WriteError(w, http.StatusRequestEntityTooLarge, "upload too large")
				return
			}
			web.WriteError(w, http.StatusBadGateway, "blobs service unreachable")
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
}
