package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestStampPublishedAt(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	nowS := now.Format(time.RFC3339)
	cases := []struct {
		name                string
		requested, existing string
		published           bool
		want                string
	}{
		{"draft, never published", "", "", false, ""},
		{"first publish stamps now", "", "", true, nowS},
		{"unpublish keeps the stamp", "", "2026-01-01T00:00:00Z", false, "2026-01-01T00:00:00Z"},
		{"republish keeps the stamp", "", "2026-01-01T00:00:00Z", true, "2026-01-01T00:00:00Z"},
		{"explicit past value wins", "2025-05-05T05:05:05Z", "2026-01-01T00:00:00Z", true, "2025-05-05T05:05:05Z"},
		{"explicit value is normalised to UTC", "2025-05-05T07:05:05+02:00", "", true, "2025-05-05T05:05:05Z"},
		{"future value is ignored", "2030-01-01T00:00:00Z", "2026-01-01T00:00:00Z", true, "2026-01-01T00:00:00Z"},
		{"garbage is ignored", "yesterday", "", true, nowS},
	}
	for _, c := range cases {
		if got := stampPublishedAt(c.requested, c.existing, c.published, now); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// openTestDB is a fresh content database with one collection.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "content.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := insertCollection(db, &Collection{Slug: "blog", Name: "Blog"}); err != nil {
		t.Fatalf("collection: %v", err)
	}
	return db
}

// TestPublishedAtLifecycle walks the stamp through every write path: blank
// on a draft, set on the first publish, sticky across unpublish and
// republish, overridable by an explicit value, deaf to a future one.
func TestPublishedAtLifecycle(t *testing.T) {
	db := openTestDB(t)
	e := &Entry{Collection: "blog", Slug: "life", Title: "Life", Body: "x"}
	if err := insertEntry(db, e); err != nil {
		t.Fatal(err)
	}
	if e.PublishedAt != "" {
		t.Fatalf("draft insert stamped %q", e.PublishedAt)
	}
	e.Published = true
	if err := updateEntry(db, e.Slug, e); err != nil {
		t.Fatal(err)
	}
	stamp := e.PublishedAt
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Fatalf("first publish stamp %q: %v", stamp, err)
	}
	if got, _ := getEntry(db, e.Slug); got.PublishedAt != stamp {
		t.Fatalf("stored %q, returned %q", got.PublishedAt, stamp)
	}
	for _, pub := range []bool{false, true, false, true} {
		e.Published = pub
		e.PublishedAt = "" // an omitted field, as every form and sync post sends
		if err := updateEntry(db, e.Slug, e); err != nil {
			t.Fatal(err)
		}
		if e.PublishedAt != stamp {
			t.Fatalf("published=%v moved the stamp to %q (was %q)", pub, e.PublishedAt, stamp)
		}
	}
	e.PublishedAt = "2021-05-05T05:05:05Z"
	if err := updateEntry(db, e.Slug, e); err != nil {
		t.Fatal(err)
	}
	if e.PublishedAt != "2021-05-05T05:05:05Z" {
		t.Fatalf("explicit correction not honoured: %q", e.PublishedAt)
	}
	e.PublishedAt = time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	if err := updateEntry(db, e.Slug, e); err != nil {
		t.Fatal(err)
	}
	if e.PublishedAt != "2021-05-05T05:05:05Z" {
		t.Fatalf("future value accepted: %q", e.PublishedAt)
	}
	if got, _ := getEntry(db, e.Slug); got.PublishedAt != "2021-05-05T05:05:05Z" {
		t.Fatalf("stored %q after the future write", got.PublishedAt)
	}

	// Born published: stamped at insert, or by the explicit value.
	p := &Entry{Collection: "blog", Slug: "born", Title: "Born", Body: "x", Published: true}
	if err := insertEntry(db, p); err != nil {
		t.Fatal(err)
	}
	if p.PublishedAt == "" {
		t.Fatal("published insert left the stamp blank")
	}
	x := &Entry{Collection: "blog", Slug: "dated", Title: "Dated", Body: "x", Published: true,
		PublishedAt: "2020-02-02T02:02:02Z"}
	if err := insertEntry(db, x); err != nil {
		t.Fatal(err)
	}
	if x.PublishedAt != "2020-02-02T02:02:02Z" {
		t.Fatalf("explicit insert stamp not honoured: %q", x.PublishedAt)
	}

	// Import: a published import was published when it was created, and a
	// re-import keeps the stamp the row already has.
	im := &Entry{Collection: "blog", Slug: "imported", Title: "Imported", Body: "x",
		Published: true, CreatedAt: "2019-01-01T00:00:00Z"}
	if err := importEntry(db, im); err != nil {
		t.Fatal(err)
	}
	if im.PublishedAt != "2019-01-01T00:00:00Z" {
		t.Fatalf("import stamp = %q, want created_at", im.PublishedAt)
	}
	im.CreatedAt = "2019-06-06T00:00:00Z"
	im.PublishedAt = ""
	if err := importEntry(db, im); err != nil {
		t.Fatal(err)
	}
	if im.PublishedAt != "2019-01-01T00:00:00Z" {
		t.Fatalf("re-import moved the stamp to %q", im.PublishedAt)
	}
}

func TestReconstructPublishedAt(t *testing.T) {
	base := &Entry{Collection: "blog", Excerpt: "ex", Tags: []string{"a"}}
	created := "2026-08-01T10:00:00Z"
	at := func(d time.Duration) string {
		return time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC).Add(d).Format(time.RFC3339)
	}
	mk := func(body string, published bool, saved string) Revision {
		p := *base
		p.Title, p.Body, p.Published = "T", body, published
		return Revision{CID: entryCID(&p), Title: "T", Body: body, SavedAt: saved}
	}
	unk := func(saved string) Revision { // a save whose excerpt or tags have since changed
		return Revision{CID: "bafkreiunknown", Title: "T", Body: "?", SavedAt: saved}
	}
	pruned := make([]Revision, keepRevisions)
	for i := range pruned {
		pruned[i] = mk(fmt.Sprint(i), true, at(time.Duration(i+1)*time.Hour))
	}
	cases := []struct {
		name      string
		createdAt string
		revs      []Revision
		want      string
		exact     bool
	}{
		{"no history", created, nil, created, false},
		{"predates the log", "2026-06-01T00:00:00Z",
			[]Revision{mk("a", false, "2026-07-16T14:56:00Z"), mk("a", true, "2026-07-16T16:53:00Z")},
			"2026-06-01T00:00:00Z", false},
		{"draft then publish", created,
			[]Revision{mk("a", false, at(0)), mk("b", false, at(time.Minute)), mk("b", true, at(2*time.Minute))},
			at(2 * time.Minute), true},
		{"draft, unclassifiable, publish", created,
			[]Revision{mk("a", false, at(0)), unk(at(time.Minute)), mk("b", true, at(2*time.Minute))},
			at(2 * time.Minute), false},
		{"born published", created,
			[]Revision{mk("a", true, at(time.Second)), mk("b", true, at(time.Hour))},
			created, true},
		{"published from a log that starts late", created,
			[]Revision{mk("a", true, at(2*time.Hour))},
			created, false},
		{"published from a pruned log", created, pruned, created, false},
		{"unclassifiable then published", created,
			[]Revision{unk(at(0)), unk(at(time.Minute)), mk("b", true, at(2*time.Minute))},
			created, false},
		{"never a published save", created,
			[]Revision{unk(at(0)), mk("a", false, at(time.Minute))},
			created, false},
	}
	for _, c := range cases {
		got, how, exact := reconstructPublishedAt(base, c.createdAt, c.revs)
		if got != c.want || exact != c.exact {
			t.Errorf("%s: got %q exact=%v (%s), want %q exact=%v", c.name, got, exact, how, c.want, c.exact)
		}
	}
}

// TestBackfillPublishedAt seeds the shapes a pre-publish-date database
// holds — an imported entry, an app-born draft that was later published, a
// trashed one, a draft — clears the stamps, and checks the migration dates
// each correctly, leaves drafts alone, and is a no-op the second time.
func TestBackfillPublishedAt(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`INSERT INTO entries (collection_id, slug, title, body, published, created_at, updated_at, cid)
		VALUES (1, 'old', 'Old', 'x', 1, '2020-03-04T05:06:07Z', '2020-03-04T05:06:07Z', 'bafkreiold')`); err != nil {
		t.Fatal(err)
	}
	born := &Entry{Collection: "blog", Slug: "born", Title: "Born", Body: "x"}
	if err := insertEntry(db, born); err != nil {
		t.Fatal(err)
	}
	born.Body = "xy"
	if err := updateEntry(db, born.Slug, born); err != nil {
		t.Fatal(err)
	}
	born.Published = true
	if err := updateEntry(db, born.Slug, born); err != nil {
		t.Fatal(err)
	}
	revs, err := revisionsOldestFirst(db, born.ID)
	if err != nil || len(revs) != 3 {
		t.Fatalf("revisions: %d, %v", len(revs), err)
	}
	// Spread the saves out so the publish moment is distinguishable.
	for i, rv := range revs {
		if _, err := db.Exec(`UPDATE entry_revisions SET saved_at = ? WHERE id = ?`,
			time.Now().UTC().Add(time.Duration(i-3)*time.Hour).Format(time.RFC3339), rv.ID); err != nil {
			t.Fatal(err)
		}
	}
	trashed := &Entry{Collection: "blog", Slug: "binned", Title: "Binned", Body: "x", Published: true}
	if err := insertEntry(db, trashed); err != nil {
		t.Fatal(err)
	}
	if _, err := deleteEntry(db, trashed.Slug); err != nil {
		t.Fatal(err)
	}
	draft := &Entry{Collection: "blog", Slug: "wip", Title: "WIP", Body: "x"}
	if err := insertEntry(db, draft); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE entries SET published_at = ''`); err != nil {
		t.Fatal(err)
	}

	snapshot := func() map[string]string {
		rows, err := db.Query(`SELECT slug, published_at FROM entries`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var s, p string
			rows.Scan(&s, &p)
			out[s] = p
		}
		return out
	}
	if err := backfillPublishedAt(db); err != nil {
		t.Fatal(err)
	}
	got := snapshot()
	revs, _ = revisionsOldestFirst(db, born.ID)
	want := map[string]string{
		"old":        "2020-03-04T05:06:07Z", // imported: its created_at
		born.Slug:    revs[2].SavedAt,        // the publish save, from history
		trashed.Slug: trashed.CreatedAt,      // born published, dated even in the trash
		draft.Slug:   "",                     // never published
	}
	for slug, w := range want {
		if got[slug] != w {
			t.Errorf("%s: published_at = %q, want %q", slug, got[slug], w)
		}
	}
	if err := backfillPublishedAt(db); err != nil {
		t.Fatal(err)
	}
	for slug, p := range snapshot() {
		if got[slug] != p {
			t.Errorf("second run moved %s from %q to %q", slug, got[slug], p)
		}
	}
}

func apiGetResp(t *testing.T, srv *httptest.Server, path, token, ifNoneMatch string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestPublishedAtInAPI: drafts omit the field, published entries carry it,
// and both the list and single-entry ETags move when published_at changes
// with nothing else touched — the exact write the backfill makes.
func TestPublishedAtInAPI(t *testing.T) {
	s, seeds := readTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	var draft map[string]any
	resp := apiGetResp(t, srv, "/api/entries/"+seeds.draftSlug, "write-secret", "")
	if err := json.NewDecoder(resp.Body).Decode(&draft); err != nil {
		t.Fatal(err)
	}
	if _, has := draft["publishedAt"]; has {
		t.Errorf("draft carries publishedAt: %v", draft["publishedAt"])
	}
	var pub map[string]any
	resp = apiGetResp(t, srv, "/api/entries/"+seeds.pubSlug, "read-secret", "")
	if err := json.NewDecoder(resp.Body).Decode(&pub); err != nil {
		t.Fatal(err)
	}
	if v, _ := pub["publishedAt"].(string); v == "" {
		t.Errorf("published entry lacks publishedAt: %v", pub)
	}

	listTag := apiGetResp(t, srv, "/api/entries", "read-secret", "").Header.Get("ETag")
	oneTag := apiGetResp(t, srv, "/api/entries/"+seeds.pubSlug, "read-secret", "").Header.Get("ETag")
	if listTag == "" || oneTag == "" {
		t.Fatal("missing ETags")
	}
	if resp := apiGetResp(t, srv, "/api/entries/"+seeds.pubSlug, "read-secret", oneTag); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("matching If-None-Match: %d, want 304", resp.StatusCode)
	}
	// The backfill's write: published_at alone, updated_at and cid untouched.
	if _, err := s.db.Exec(`UPDATE entries SET published_at = '2020-01-01T00:00:00Z' WHERE slug = ?`, seeds.pubSlug); err != nil {
		t.Fatal(err)
	}
	if got := apiGetResp(t, srv, "/api/entries", "read-secret", listTag); got.StatusCode != http.StatusOK || got.Header.Get("ETag") == listTag {
		t.Errorf("list ETag did not move on a published_at change: %d %s", got.StatusCode, got.Header.Get("ETag"))
	}
	got := apiGetResp(t, srv, "/api/entries/"+seeds.pubSlug, "read-secret", oneTag)
	if got.StatusCode != http.StatusOK || got.Header.Get("ETag") == oneTag {
		t.Errorf("entry ETag did not move on a published_at change: %d %s", got.StatusCode, got.Header.Get("ETag"))
	}
	var after Entry
	if err := json.NewDecoder(got.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if after.PublishedAt != "2020-01-01T00:00:00Z" || after.CID != pub["cid"] {
		t.Errorf("after: publishedAt=%q cid=%q (cid before %v)", after.PublishedAt, after.CID, pub["cid"])
	}
}
