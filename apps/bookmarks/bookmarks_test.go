package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// newTestDB opens a fresh database in t.TempDir, runs migrations, and returns it.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "bookmarks.sqlite")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// newTestServer returns a Server backed by an in-temp-dir database, ready to
// drive with httptest. The HTTP client is stubbed so metadata fetches that
// reach out to test fixtures resolve locally; a missing host yields an error
// and the handler still saves the bookmark.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	db := newTestDB(t)
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	return &Server{
		db:           db,
		templates:    tmpl,
		password:     "secret",
		apiKey:       "k1",
		cookieSecure: false,
		http:         &http.Client{},
		assetVer:     "test",
	}
}

func TestBookmarkCRUD(t *testing.T) {
	db := newTestDB(t)

	b := &Bookmark{
		URL:         "https://example.com/",
		Title:       "Example",
		Description: "An example.",
		Category:    "Reference",
		Public:      true,
		AdminNotes:  "private note",
	}
	if err := insertBookmark(db, b); err != nil {
		t.Fatalf("insertBookmark: %v", err)
	}
	if b.ID == "" {
		t.Fatal("insertBookmark left ID empty")
	}
	if b.CID == "" {
		t.Fatal("insertBookmark left CID empty")
	}

	got, err := getBookmark(db, b.ID)
	if err != nil {
		t.Fatalf("getBookmark: %v", err)
	}
	if got == nil {
		t.Fatal("getBookmark returned nil")
	}
	if got.URL != b.URL || got.Title != b.Title || !got.Public ||
		got.AdminNotes != "private note" {
		t.Errorf("getBookmark round-trip mismatch: %+v", got)
	}

	b.Title = "Edited"
	b.Public = false
	ok, err := updateBookmark(db, b.ID, b)
	if err != nil || !ok {
		t.Fatalf("updateBookmark: ok=%v err=%v", ok, err)
	}
	got, _ = getBookmark(db, b.ID)
	if got.Title != "Edited" || got.Public {
		t.Errorf("updateBookmark did not persist: %+v", got)
	}
	if got.CID == "" {
		t.Error("updateBookmark cleared CID")
	}

	deleted, err := deleteBookmark(db, b.ID)
	if err != nil || !deleted {
		t.Fatalf("deleteBookmark: ok=%v err=%v", deleted, err)
	}
	got, _ = getBookmark(db, b.ID)
	if got != nil {
		t.Error("deleteBookmark left the row behind")
	}
}

func TestListPublicFilter(t *testing.T) {
	db := newTestDB(t)
	must := func(b *Bookmark) {
		if err := insertBookmark(db, b); err != nil {
			t.Fatalf("insert %s: %v", b.URL, err)
		}
	}
	must(&Bookmark{URL: "https://a.example", Category: "A", Public: true})
	must(&Bookmark{URL: "https://b.example", Category: "B", Public: false})
	must(&Bookmark{URL: "https://c.example", Category: "A", Public: true})

	all, err := listBookmarks(db)
	if err != nil {
		t.Fatalf("listBookmarks: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("listBookmarks = %d, want 3", len(all))
	}

	pub, err := listPublicBookmarks(db)
	if err != nil {
		t.Fatalf("listPublicBookmarks: %v", err)
	}
	if len(pub) != 2 {
		t.Fatalf("listPublicBookmarks = %d, want 2", len(pub))
	}
	for _, b := range pub {
		if !b.Public {
			t.Errorf("private bookmark leaked into public list: %s", b.URL)
		}
	}
}

func TestPublicListStripsAdminNotes(t *testing.T) {
	db := newTestDB(t)
	if err := insertBookmark(db, &Bookmark{
		URL: "https://a.example", Public: true, AdminNotes: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	pub, _ := listPublicBookmarks(db)
	stripped := publicList(pub)
	if stripped[0].AdminNotes != "" {
		t.Errorf("publicList kept AdminNotes: %q", stripped[0].AdminNotes)
	}
	if pub[0].AdminNotes != "secret" {
		t.Errorf("publicList mutated source list: %q", pub[0].AdminNotes)
	}
}

func TestCIDExcludesAdminNotesAndTimestamps(t *testing.T) {
	a := &Bookmark{URL: "https://x.example", Title: "X", Public: true, AdminNotes: "a"}
	b := &Bookmark{URL: "https://x.example", Title: "X", Public: true, AdminNotes: "b"}
	if bookmarkCID(a) != bookmarkCID(b) {
		t.Errorf("CID changed when admin notes did — should not")
	}
	c := &Bookmark{URL: "https://x.example", Title: "X", Public: true,
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-05-20T00:00:00Z"}
	if bookmarkCID(a) != bookmarkCID(c) {
		t.Errorf("CID changed when timestamps did — should not")
	}
	d := &Bookmark{URL: "https://x.example", Title: "Y", Public: true}
	if bookmarkCID(a) == bookmarkCID(d) {
		t.Errorf("CID stayed equal when title changed — should differ")
	}
}

func TestExtractMetaFromHTML(t *testing.T) {
	html := []byte(`<!doctype html>
<html><head>
<title>  Example &amp; Co.  </title>
<meta name="description" content="A great page.">
<meta name="author" content="Ada Lovelace">
<meta property="og:title" content="OG Title">
<meta property="og:description" content="OG Desc">
<meta property="og:image" content="/img/cover.jpg">
<meta property="og:site_name" content="Example">
<meta property="og:type" content="website">
<link rel="icon" href="/favicon.ico">
<script>var x = "<title>fake</title>";</script>
</head><body>body</body></html>`)
	base, _ := url.Parse("https://example.com/path/")
	m := extractMeta(html, base)
	if m.Title != "Example & Co." {
		t.Errorf("Title = %q, want %q", m.Title, "Example & Co.")
	}
	if m.Description != "A great page." {
		t.Errorf("Description = %q", m.Description)
	}
	if m.Author != "Ada Lovelace" {
		t.Errorf("Author = %q", m.Author)
	}
	if m.OGTitle != "OG Title" || m.OGDescription != "OG Desc" {
		t.Errorf("OG title/desc not extracted: %+v", m)
	}
	if m.OGImage != "https://example.com/img/cover.jpg" {
		t.Errorf("OG image not resolved: %q", m.OGImage)
	}
	if m.OGSiteName != "Example" || m.OGType != "website" {
		t.Errorf("OG site/type missing: %+v", m)
	}
	if m.Favicon != "https://example.com/favicon.ico" {
		t.Errorf("Favicon = %q", m.Favicon)
	}
}

func TestExtractMetaFallback(t *testing.T) {
	// Only a <title> — description, OG, favicon all absent. The well-known
	// /favicon.ico still falls in as the default guess.
	html := []byte(`<html><head><title>Plain</title></head><body></body></html>`)
	base, _ := url.Parse("https://example.org/")
	m := extractMeta(html, base)
	if m.Title != "Plain" {
		t.Errorf("Title fallback = %q", m.Title)
	}
	if m.OGTitle != "" || m.Description != "" {
		t.Errorf("expected empty OG/description fallback, got %+v", m)
	}
	if m.Favicon != "https://example.org/favicon.ico" {
		t.Errorf("Favicon default = %q", m.Favicon)
	}
}

func TestApplyMetadataPrefersAdminTitle(t *testing.T) {
	b := &Bookmark{URL: "https://x.example", Title: "Admin Title"}
	applyMetadata(b, metaResult{Title: "From Page", OGTitle: "From OG"})
	if b.Title != "Admin Title" {
		t.Errorf("applyMetadata clobbered admin Title: %q", b.Title)
	}
	if b.OGTitle != "From OG" {
		t.Errorf("OGTitle not set: %q", b.OGTitle)
	}
}

func TestApplyMetadataFillsEmptyTitle(t *testing.T) {
	b := &Bookmark{URL: "https://x.example"}
	applyMetadata(b, metaResult{Title: "Page", OGTitle: "OG"})
	if b.Title != "OG" {
		t.Errorf("Title should prefer OG: %q", b.Title)
	}
	b2 := &Bookmark{URL: "https://x.example"}
	applyMetadata(b2, metaResult{Title: "Page"})
	if b2.Title != "Page" {
		t.Errorf("Title should fall back to <title>: %q", b2.Title)
	}
}

func TestGroupByCategory(t *testing.T) {
	in := []Bookmark{
		{ID: "1", Category: "B"},
		{ID: "2", Category: "A"},
		{ID: "3", Category: ""},
		{ID: "4", Category: "A"},
	}
	groups := groupByCategory(in)
	// Expect input order to determine first-seen category order (B, A) and
	// Uncategorized last.
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3", len(groups))
	}
	if groups[0].Name != "B" || groups[1].Name != "A" || groups[2].Name != "Uncategorized" {
		t.Errorf("group order = [%s %s %s]", groups[0].Name, groups[1].Name, groups[2].Name)
	}
	if len(groups[1].Bookmarks) != 2 {
		t.Errorf("category A: got %d bookmarks", len(groups[1].Bookmarks))
	}
}

func TestPublicAPISkipsPrivateAndStripsNotes(t *testing.T) {
	s := newTestServer(t)
	mustInsert := func(b *Bookmark) {
		if err := insertBookmark(s.db, b); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	mustInsert(&Bookmark{URL: "https://a.example", Public: true, AdminNotes: "secret-a"})
	priv := &Bookmark{URL: "https://b.example", Public: false, AdminNotes: "secret-b"}
	mustInsert(priv)

	// public list
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/bookmarks")
	if err != nil {
		t.Fatalf("GET list: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("GET list status %d", resp.StatusCode)
	}
	var listResp struct {
		Bookmarks []Bookmark `json:"bookmarks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if len(listResp.Bookmarks) != 1 {
		t.Fatalf("public list = %d, want 1 (private must be excluded)", len(listResp.Bookmarks))
	}
	if listResp.Bookmarks[0].AdminNotes != "" {
		t.Errorf("public list leaked AdminNotes: %q", listResp.Bookmarks[0].AdminNotes)
	}

	// ETag round-trip on list endpoint
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("list endpoint missing ETag")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/bookmarks", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET: %v", err)
	}
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional list = %d, want 304", resp2.StatusCode)
	}
	resp2.Body.Close()

	// Private GET must 404
	resp3, _ := http.Get(ts.URL + "/api/bookmarks/" + priv.ID)
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("private GET = %d, want 404", resp3.StatusCode)
	}
	resp3.Body.Close()
}

func TestSingleRecordETag(t *testing.T) {
	s := newTestServer(t)
	b := &Bookmark{URL: "https://e.example", Public: true, Title: "E"}
	if err := insertBookmark(s.db, b); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/bookmarks/" + b.ID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" || !strings.Contains(etag, b.CID) {
		t.Errorf("ETag = %q, want CID %q", etag, b.CID)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), `"adminNotes"`) {
		t.Errorf("single GET leaked adminNotes: %s", body)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/bookmarks/"+b.ID, nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional single = %d, want 304", resp2.StatusCode)
	}
}

func TestAdminAuthSmoke(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	// Unauthenticated /admin redirects to /admin/login. We disable redirect
	// following so we can read the Location header.
	c := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Jar: nil,
	}
	resp, err := c.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatalf("GET /admin: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("/admin status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "/admin/login") {
		t.Errorf("/admin Location = %q, want /admin/login", loc)
	}
	resp.Body.Close()

	// A wrong password redirects back to /admin/login?error=...
	resp2, err := c.PostForm(ts.URL+"/admin/login",
		url.Values{"password": {"wrong"}})
	if err != nil {
		t.Fatalf("POST wrong: %v", err)
	}
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("wrong login status = %d, want 303", resp2.StatusCode)
	}
	if loc := resp2.Header.Get("Location"); !strings.Contains(loc, "error") {
		t.Errorf("wrong login Location = %q, want error param", loc)
	}
	resp2.Body.Close()

	// Correct password sets a cookie and redirects to /admin.
	resp3, err := c.PostForm(ts.URL+"/admin/login",
		url.Values{"password": {"secret"}})
	if err != nil {
		t.Fatalf("POST right: %v", err)
	}
	if resp3.StatusCode != http.StatusSeeOther {
		t.Errorf("login status = %d, want 303", resp3.StatusCode)
	}
	cookies := resp3.Cookies()
	resp3.Body.Close()
	if len(cookies) == 0 {
		t.Fatal("login did not set a cookie")
	}

	// Use the cookie to reach /admin.
	req, _ := http.NewRequest("GET", ts.URL+"/admin", nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	resp4, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET /admin with cookie: %v", err)
	}
	defer resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK {
		t.Errorf("/admin authed status = %d, want 200", resp4.StatusCode)
	}
}

func TestStatusEndpoint(t *testing.T) {
	s := newTestServer(t)
	if err := insertBookmark(s.db, &Bookmark{URL: "https://a.example", Public: true}); err != nil {
		t.Fatal(err)
	}
	if err := insertBookmark(s.db, &Bookmark{URL: "https://b.example", Public: false}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Service   string `json:"service"`
		OK        bool   `json:"ok"`
		Bookmarks int    `json:"bookmarks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Service != "bookmarks" || !body.OK {
		t.Errorf("status = %+v", body)
	}
	if body.Bookmarks != 1 {
		t.Errorf("status counts only public bookmarks: got %d, want 1", body.Bookmarks)
	}
}
