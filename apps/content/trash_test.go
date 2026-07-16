package main

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
)

// sessionCookie mints a valid admin session for HTML-route tests.
func sessionCookie(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	tok := auth.NewSessionToken()
	if err := store.InsertSession(s.db, tok, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return &http.Cookie{Name: "session", Value: tok}
}

// noRedirect keeps 303s observable instead of following them.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

func adminGet(t *testing.T, srv *httptest.Server, cookie *http.Cookie, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.AddCookie(cookie)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func adminPost(t *testing.T, srv *httptest.Server, cookie *http.Cookie, path string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestTrashLifecycle walks the whole soft-delete flow over HTTP: a deleted
// entry vanishes from the public API and the admin list, shows up in the
// trash, comes back on restore, and is gone for good (revisions included)
// after delete-forever.
func TestTrashLifecycle(t *testing.T) {
	s, ids := readTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	cookie := sessionCookie(t, s)

	var entryID int64
	if err := s.db.QueryRow(`SELECT id FROM entries WHERE slug = ?`, ids.pubSlug).Scan(&entryID); err != nil {
		t.Fatalf("entry id: %v", err)
	}

	// API DELETE is a soft delete with the response shape unchanged.
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/entries/"+ids.pubSlug, nil)
	req.Header.Set("Authorization", "Bearer write-secret")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), ids.pubSlug) {
		t.Fatalf("API delete = %d %s, want 200 with deleted slug", resp.StatusCode, body)
	}

	// Hidden from the public API — the single read and the list.
	if code, _ := apiGet(t, srv, "/api/entries/"+ids.pubSlug, ""); code != http.StatusNotFound {
		t.Errorf("trashed entry fetch = %d, want 404", code)
	}
	if _, listBody := apiGet(t, srv, "/api/entries", "read-secret"); len(entrySlugs(t, listBody)) != 0 {
		t.Errorf("trashed entry still in public list: %s", listBody)
	}

	// Hidden from the admin entries list, present on the trash page.
	if _, page := adminGet(t, srv, cookie, "/entries"); strings.Contains(page, ids.pubSlug) {
		t.Errorf("trashed entry still on the admin entries page")
	}
	if code, page := adminGet(t, srv, cookie, "/entries/trash"); code != http.StatusOK || !strings.Contains(page, ids.pubSlug) {
		t.Errorf("trash page = %d, want 200 listing %q", code, ids.pubSlug)
	}

	// Deleting an already-trashed entry reads as missing.
	req, _ = http.NewRequest("DELETE", srv.URL+"/api/entries/"+ids.pubSlug, nil)
	req.Header.Set("Authorization", "Bearer write-secret")
	if resp, err := srv.Client().Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("double delete = %d, want 404", resp.StatusCode)
		}
	}

	// Restore brings it back everywhere.
	if code := adminPost(t, srv, cookie, "/entries/"+ids.pubSlug+"/restore"); code != http.StatusSeeOther {
		t.Fatalf("restore = %d, want 303", code)
	}
	if code, _ := apiGet(t, srv, "/api/entries/"+ids.pubSlug, ""); code != http.StatusOK {
		t.Errorf("restored entry fetch = %d, want 200", code)
	}
	if _, page := adminGet(t, srv, cookie, "/entries/trash"); strings.Contains(page, ids.pubSlug) {
		t.Errorf("restored entry still on the trash page")
	}

	// Delete forever only works from the trash: a live entry survives it.
	if code := adminPost(t, srv, cookie, "/entries/"+ids.pubSlug+"/destroy"); code != http.StatusSeeOther {
		t.Fatalf("destroy (live) = %d, want 303", code)
	}
	if e, _ := getEntry(s.db, ids.pubSlug); e == nil {
		t.Fatalf("destroy skipped the trash and removed a live entry")
	}

	// Trash via the admin route, then delete forever — row and revisions gone.
	if code := adminPost(t, srv, cookie, "/entries/"+ids.pubSlug+"/delete"); code != http.StatusSeeOther {
		t.Fatalf("admin delete = %d, want 303", code)
	}
	if code := adminPost(t, srv, cookie, "/entries/"+ids.pubSlug+"/destroy"); code != http.StatusSeeOther {
		t.Fatalf("destroy = %d, want 303", code)
	}
	var rows, revs int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM entries WHERE slug = ?`, ids.pubSlug).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM entry_revisions WHERE entry_id = ?`, entryID).Scan(&revs); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || revs != 0 {
		t.Errorf("after destroy: %d rows, %d revisions, want 0 and 0", rows, revs)
	}
}

// TestTrashHiddenFromReads pins the db-level contract: a trashed entry is
// invisible to every read helper, uneditable, and excluded from counts and
// fingerprints until restored.
func TestTrashHiddenFromReads(t *testing.T) {
	s, ids := readTestServer(t)

	before, err := entriesFingerprint(s.db, "", statusAll)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := deleteEntry(s.db, ids.pubSlug); err != nil || !ok {
		t.Fatalf("deleteEntry = %v, %v", ok, err)
	}

	if e, err := getEntry(s.db, ids.pubSlug); err != nil || e != nil {
		t.Errorf("getEntry(trashed) = %v, %v, want nil, nil", e, err)
	}
	if err := updateEntry(s.db, ids.pubSlug, &Entry{
		Collection: "blog", Slug: ids.pubSlug, Title: "Edited",
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("updateEntry(trashed) = %v, want sql.ErrNoRows", err)
	}
	for _, status := range []entryStatus{statusAll, statusPublished} {
		entries, err := listEntries(s.db, "", status, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Slug == ids.pubSlug {
				t.Errorf("trashed entry leaked into listEntries(status=%d)", status)
			}
		}
	}
	if n, _ := countEntries(s.db, "", statusAll); n != 1 {
		t.Errorf("countEntries after trash = %d, want 1 (the draft)", n)
	}
	if fp, _ := entriesFingerprint(s.db, "", statusAll); fp == before {
		t.Errorf("fingerprint unchanged by a trash — stale ETags would leak the old list")
	}
	cols, err := listCollections(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || cols[0].EntryCount != 1 {
		t.Errorf("collection entry count = %+v, want 1 visible entry", cols)
	}

	trashed, err := listTrash(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(trashed) != 1 || trashed[0].Slug != ids.pubSlug || trashed[0].DeletedAt == "" {
		t.Errorf("listTrash = %+v, want the trashed entry with its deleted date", trashed)
	}

	if ok, err := restoreEntry(s.db, ids.pubSlug); err != nil || !ok {
		t.Fatalf("restoreEntry = %v, %v", ok, err)
	}
	if e, err := getEntry(s.db, ids.pubSlug); err != nil || e == nil {
		t.Errorf("getEntry(restored) = %v, %v, want the entry back", e, err)
	}
}

// TestPurgeTrash confirms the startup purge reaps only entries past the
// retention window.
func TestPurgeTrash(t *testing.T) {
	s, ids := readTestServer(t)

	if ok, err := deleteEntry(s.db, ids.pubSlug); err != nil || !ok {
		t.Fatalf("deleteEntry(pub) = %v, %v", ok, err)
	}
	if ok, err := deleteEntry(s.db, ids.draftSlug); err != nil || !ok {
		t.Fatalf("deleteEntry(draft) = %v, %v", ok, err)
	}
	// Age one past the retention window.
	old := time.Now().UTC().Add(-trashRetention - time.Hour).Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE entries SET deleted_at = ? WHERE slug = ?`, old, ids.pubSlug); err != nil {
		t.Fatal(err)
	}

	if err := purgeTrash(s.db); err != nil {
		t.Fatalf("purgeTrash: %v", err)
	}
	trashed, err := listTrash(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(trashed) != 1 || trashed[0].Slug != ids.draftSlug {
		t.Errorf("after purge trash = %+v, want only the fresh %q", trashed, ids.draftSlug)
	}
}
