package web

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// proxyClient calls sibling farfield services. The timeout is generous
// because these requests carry media bytes.
var proxyClient = &http.Client{Timeout: 60 * time.Second}

// MaxEmbedUpload caps a proxied upload — it matches the blobs service's own
// limit, so a proxy never accepts more than blobs would.
const MaxEmbedUpload = 100 << 20 // 100 MiB

// Server-side proxies for the admin editors. content and feed both let a
// session-gated page reach a sibling service — uploading to blobs, reading the
// token-gated blob index, listing series — without the API key ever reaching
// the browser. Both apps grew their own copy of this plumbing; it lives here
// so there is one.

// ProxyGet forwards a GET (with its query string) to an internal farfield
// service using the server-side API key, streaming the JSON response back.
func ProxyGet(w http.ResponseWriter, r *http.Request, target, apiKey string) {
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "bad upstream request")
		return
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := proxyClient.Do(req)
	if err != nil {
		WriteError(w, http.StatusBadGateway, "upstream unreachable")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// ProxyUpload forwards a browser multipart upload (the "file" part) to the
// blobs service as raw bytes with the API key attached, and relays the
// response.
//
// The part streams straight through as the upstream request body: reading it
// with a MultipartReader rather than ParseMultipartForm means the upload is
// never buffered, in memory or on disk, however large it is. maxBytes bounds
// the request so the proxy never accepts more than blobs itself would.
func ProxyUpload(w http.ResponseWriter, r *http.Request, blobsURL, apiKey string, maxBytes int64) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid upload")
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			WriteError(w, http.StatusBadRequest, "missing file")
			return
		}
		if err != nil {
			WriteError(w, http.StatusBadRequest, "invalid upload")
			return
		}
		if part.FormName() != "file" {
			continue
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
			strings.TrimRight(blobsURL, "/")+"/blobs", part)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "could not build request")
			return
		}
		req.Header.Set("X-API-Key", apiKey)
		if ct := part.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := proxyClient.Do(req)
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				WriteError(w, http.StatusRequestEntityTooLarge, "upload too large")
				return
			}
			WriteError(w, http.StatusBadGateway, "blobs service unreachable")
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
}
