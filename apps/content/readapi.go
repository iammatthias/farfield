package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/web"
)

// The public JSON read API the site builds against: collections, entries,
// series. Enumerating reads are token-gated; a single entry by slug is public
// but rate-limited, so a "view source" link opens in a browser.

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

// The public JSON read API the site builds against: collections, entries,
// series. Enumerating reads are token-gated; a single entry by slug is public
// but rate-limited, so a "view source" link opens in a browser.

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countCollections(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "content", "ok": true, "collections": n,
	})
}

func (s *Server) handleAPICollections(w http.ResponseWriter, r *http.Request) {
	collections, err := listCollections(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list collections")
		return
	}
	if collections == nil {
		collections = []Collection{}
	}
	// Collections lack an updated_at column, so there is no cheap pre-query
	// fingerprint that catches renames — the ETag comes from the loaded rows
	// instead. The rows are tiny; the 304 saves serialization and bandwidth.
	web.WriteRecord(w, r, cid.OfValue(collections), map[string]any{"collections": collections})
}

func (s *Server) handleAPIEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	collection := q.Get("collection")
	status, ok := s.resolveStatus(w, r, q.Get("status"))
	if !ok {
		return
	}
	limit, page := parsePaging(q.Get("limit"), q.Get("page"))
	// ?bodies=0 is the slim list: everything but the markdown bodies, for
	// index surfaces that only need title/excerpt/tags. The flag is part of
	// the ETag input — a slim response must never satisfy a full request's
	// revalidation, or a client would keep a bodyless copy believing it full.
	slim := q.Get("bodies") == "0"

	// List-level ETag from a cheap fingerprint, checked before the full list
	// query — an unchanged client revalidates without a single body loading.
	// The status is in the fingerprint so a draft view never reuses a published
	// list's ETag.
	fp, err := entriesFingerprint(s.db, collection, status)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list entries")
		return
	}
	if listETagDone(w, r, fmt.Sprintf("entries|%s|%d|%d|%d|%v|%s", collection, int(status), limit, page, slim, fp)) {
		return
	}

	offset := 0
	if limit > 0 {
		offset = (page - 1) * limit
	}
	entries, err := listEntriesFull(s.db, collection, status, limit, offset)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list entries")
		return
	}
	if entries == nil {
		entries = []Entry{}
	}
	if slim {
		for i := range entries {
			entries[i].Body = ""
		}
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// resolveStatus maps the ?status= query to an entryStatus and enforces that
// draft visibility (status=draft or status=all) requires the write key — the
// read token alone only ever sees published content. It writes the error
// response and returns ok=false when the request is rejected.
func (s *Server) resolveStatus(w http.ResponseWriter, r *http.Request, param string) (entryStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(param)) {
	case "", "published":
		return statusPublished, true
	case "draft", "drafts":
		if !s.auth.HasWriteKey(r) {
			web.WriteError(w, http.StatusForbidden, "drafts require the write API key")
			return statusPublished, false
		}
		return statusDraft, true
	case "all":
		if !s.auth.HasWriteKey(r) {
			web.WriteError(w, http.StatusForbidden, "drafts require the write API key")
			return statusPublished, false
		}
		return statusAll, true
	default:
		web.WriteError(w, http.StatusBadRequest, "unknown status (use published, draft, or all)")
		return statusPublished, false
	}
}

// maxPageSize caps a requested page of /api/entries.
const maxPageSize = 500

// parsePaging reads ?limit= and ?page= (1-based). No params means the full
// list (limit 0) — the original, backward-compatible response. An explicit
// limit is capped at maxPageSize; a page without a limit implies the cap.
func parsePaging(limitStr, pageStr string) (limit, page int) {
	if limitStr != "" {
		limit, _ = strconv.Atoi(limitStr)
	}
	page = 1
	if p, err := strconv.Atoi(pageStr); err == nil && p > 1 {
		page = p
	}
	if limit <= 0 {
		limit = 0
		if page > 1 {
			limit = maxPageSize
		}
	} else if limit > maxPageSize {
		limit = maxPageSize
	}
	return limit, page
}

// listETagDone hashes fingerprint into a list-level ETag, sets the caching
// headers, and reports whether the request was satisfied with a 304.
func listETagDone(w http.ResponseWriter, r *http.Request, fingerprint string) bool {
	etag := cid.Of([]byte(fingerprint))
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	if web.ETagMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

func (s *Server) handleAPIEntry(w http.ResponseWriter, r *http.Request) {
	e, err := getEntry(s.db, r.PathValue("slug"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read entry")
		return
	}
	// A draft is fetchable by slug only with the write key — that is the
	// preview path. To the read token a draft is indistinguishable from a
	// missing entry.
	if e == nil || (!e.Published && !s.auth.HasWriteKey(r)) {
		web.WriteError(w, http.StatusNotFound, "entry not found")
		return
	}
	// Not the bare CID: published_at sits outside it, and a date filled in
	// by backfill or by hand must invalidate a cached copy too.
	web.WriteRecord(w, r, entryETag(e), e)
}
