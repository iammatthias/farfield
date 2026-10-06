package web

import (
	"net/http"
	"strings"
)

// Optimistic concurrency for the JSON write APIs. A client that read a record
// (and its ETag) sends the tag back as If-Match on the write; if anyone else
// saved in between, the write is refused with 412 and the record as it now
// stands, instead of silently overwriting the other save. Without the header
// every write behaves exactly as it always has — last write wins.
//
// The check here is only half of it. Comparing tags in Go and then writing is
// a race on its own; the caller must also guard the write itself on the
// version it checked (UPDATE ... WHERE cid = ?) and treat zero rows affected
// as a lost race — see WritePreconditionFailed.

// Precondition is the outcome of an If-Match check.
type Precondition int

const (
	// Unconditional: no If-Match (or "*", which any existing record
	// satisfies). Write exactly as before, unguarded.
	Unconditional Precondition = iota
	// Matched: the client holds the current version. Guard the write on that
	// version so a save landing between the check and the write still loses.
	Matched
	// Failed: the client's version is stale. The 412 has been written.
	Failed
)

// CheckIfMatch compares the request's If-Match against etag, the record's
// current validator — the same unquoted value its GET sends as ETag. On a
// mismatch it writes the 412 (with current as the body's "current") and
// returns Failed; the caller just returns. The comparison is ETagMatch's:
// weak, quoted, and listed validators all count, because a client may only
// ever have seen the tag after a proxy rewrote it.
//
// The caller has already established the record exists — If-Match on a
// missing record is the caller's 404, not a 412.
func CheckIfMatch(w http.ResponseWriter, r *http.Request, etag string, current any) Precondition {
	header := strings.TrimSpace(r.Header.Get("If-Match"))
	switch {
	case header == "" || header == "*":
		return Unconditional
	case etagListMatch(header, etag):
		return Matched
	}
	WritePreconditionFailed(w, etag, current)
	return Failed
}

// WritePreconditionFailed answers 412 with the record as it currently stands
// and its ETag, so the client can show the conflict or retry against the new
// version without a second round trip. Handlers call it directly when a
// guarded write affected no rows — the race CheckIfMatch alone cannot see.
func WritePreconditionFailed(w http.ResponseWriter, etag string, current any) {
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, http.StatusPreconditionFailed, map[string]any{
		"error":   "precondition failed",
		"current": current,
	})
}

// WriteSaved writes a record a write just produced, with its new ETag — the
// value a GET of the record would now send — so a client can chain the next
// conditional write without re-reading.
func WriteSaved(w http.ResponseWriter, status int, etag string, v any) {
	w.Header().Set("ETag", `"`+etag+`"`)
	WriteJSON(w, status, v)
}
