package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
)

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes a JSON error body with the given status.
func WriteError(w http.ResponseWriter, status int, msg string) {
	// Errors are diagnostic, never content: an edge cache rule that holds a
	// 404 would keep answering "not found" for hours after the thing exists
	// (observed live once a /blobs/* cache rule went in front of the fleet).
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, status, map[string]string{"error": msg})
}

// ETagMatch reports whether the request's If-None-Match header matches etag
// (unquoted). It accepts weak validators (W/"...") — proxies like Cloudflare
// rewrite strong tags to weak when they re-encode a response — and
// comma-separated candidate lists, which an exact string compare misses.
func ETagMatch(r *http.Request, etag string) bool {
	return etagListMatch(r.Header.Get("If-None-Match"), etag)
}

// etagListMatch is the validator comparison both conditional headers share:
// "*", or any candidate in a comma-separated list, weak or strong, quoted or
// not. An empty header matches nothing.
func etagListMatch(header, etag string) bool {
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

// WriteJSONValidated writes v as a 200 JSON response whose ETag is the CID of
// the response bytes themselves, answering 304 Not Modified when the client
// already holds them. It is for list reads that have no single record version
// to hand out: a tag derived from the bytes is correct by construction — any
// change to any row, page, or query changes the body and so the tag — with no
// version bookkeeping to keep in step with the writes.
//
// The body is byte-identical to WriteJSON's (Encode's trailing newline
// included), so switching a route over changes no client's parse. It sets no
// Cache-Control: each route keeps its own (PrivateAPI's no-store still wins,
// and 304 still pays off for a client that keeps its own copy).
func WriteJSONValidated(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "could not encode response")
		return
	}
	body = append(body, '\n')
	etag := cid.Of(body)
	w.Header().Set("ETag", `"`+etag+`"`)
	if ETagMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
