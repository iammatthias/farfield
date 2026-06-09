package web

import (
	"encoding/json"
	"net/http"
	"strings"
)

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes a JSON error body with the given status.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// ETagMatch reports whether the request's If-None-Match header matches etag
// (unquoted). It accepts weak validators (W/"...") — proxies like Cloudflare
// rewrite strong tags to weak when they re-encode a response — and
// comma-separated candidate lists, which an exact string compare misses.
func ETagMatch(r *http.Request, etag string) bool {
	header := r.Header.Get("If-None-Match")
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		candidate = strings.Trim(candidate, `"`)
		if candidate == etag {
			return true
		}
	}
	return false
}

// WriteRecord writes v as JSON with etag (typically a content CID) as its
// ETag, short-circuiting to 304 Not Modified when the client already holds
// that version. Cache-Control: no-cache makes the revalidation contract
// explicit — cache, but always check back.
func WriteRecord(w http.ResponseWriter, r *http.Request, etag string, v any) {
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	if ETagMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	WriteJSON(w, http.StatusOK, v)
}
