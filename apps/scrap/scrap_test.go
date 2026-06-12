package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── helpers ────────────────────────────────────────────────────────────────

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "scrap.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := newServer(newTestDB(t), "secret", "k1", false, "https://scrap.test")
	if err := s.parseTemplates(); err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func loginSession(t *testing.T, ts *httptest.Server) []*http.Cookie {
	t.Helper()
	resp, err := noRedirectClient().PostForm(ts.URL+"/login",
		url.Values{"password": {"secret"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a cookie")
	}
	return cookies
}

// apiCreate posts a raw body through the terminal API and returns the
// response status and text.
func apiCreate(t *testing.T, ts *httptest.Server, body, query string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/api/pastes"+query,
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-API-Key", "k1")
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("api create: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// createdID extracts the paste id from an API create response body.
func createdID(t *testing.T, out string) string {
	t.Helper()
	first, _, _ := strings.Cut(out, "\n")
	id := strings.TrimPrefix(strings.TrimSpace(first), "https://scrap.test/")
	if !validID(id) {
		t.Fatalf("response %q did not yield a valid id", out)
	}
	return id
}

func get(t *testing.T, rawURL string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// ── create gates ───────────────────────────────────────────────────────────

func TestComposeEmptyBodyIs400(t *testing.T) {
	_, ts := newTestServer(t)
	cookies := loginSession(t, ts)

	req, _ := http.NewRequest("POST", ts.URL+"/pastes",
		strings.NewReader(url.Values{"body": {"   "}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "Body is required") {
		t.Error("response does not carry the inline error")
	}
}

func TestUnauthenticatedCreateIsRefused(t *testing.T) {
	_, ts := newTestServer(t)

	// Browser create without a session redirects to login.
	resp, err := noRedirectClient().PostForm(ts.URL+"/pastes",
		url.Values{"body": {"hello"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("browser status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Fatalf("redirect = %q, want /login", loc)
	}

	// API create without a key is a 401.
	resp2, err := http.Post(ts.URL+"/api/pastes", "text/plain",
		strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("api post: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api status = %d, want 401", resp2.StatusCode)
	}
}

func TestAPIEmptyBodyIs400(t *testing.T) {
	_, ts := newTestServer(t)
	status, _ := apiCreate(t, ts, "", "")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

// ── terminal API ───────────────────────────────────────────────────────────

func TestPipeReturnsBareURL(t *testing.T) {
	_, ts := newTestServer(t)
	status, out := apiCreate(t, ts, "package main\n", "?lang=go")
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}
	if !strings.HasPrefix(out, "https://scrap.test/") || !strings.HasSuffix(out, "\n") {
		t.Fatalf("body = %q, want a bare absolute URL ending in newline", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("body = %q, want exactly one line", out)
	}
}

func TestGeneratedTokenIsReturnedOnSecondLine(t *testing.T) {
	_, ts := newTestServer(t)
	status, out := apiCreate(t, ts, "locked content", "?token=generate")
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "token: ") {
		t.Fatalf("body = %q, want URL line + token line", out)
	}
	if secret := strings.TrimPrefix(lines[1], "token: "); len(secret) != 26 {
		t.Fatalf("token %q, want 26 chars", secret)
	}
}

func TestSameBodyDedupsToSameID(t *testing.T) {
	s, ts := newTestServer(t)
	_, out1 := apiCreate(t, ts, "same body", "?title=first&visibility=public")
	_, out2 := apiCreate(t, ts, "same body", "?title=second")
	id1, id2 := createdID(t, out1), createdID(t, out2)
	if id1 != id2 {
		t.Fatalf("ids differ: %q vs %q", id1, id2)
	}
	n, err := countPastes(s.db)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	// Metadata upserted; views preserved.
	p, err := getPaste(s.db, id1)
	if err != nil || p == nil {
		t.Fatalf("getPaste: %v, %v", p, err)
	}
	if p.Title != "second" || p.Visibility != VisUnlisted {
		t.Fatalf("metadata not upserted: title=%q vis=%q", p.Title, p.Visibility)
	}
}

func TestAPIDelete(t *testing.T) {
	_, ts := newTestServer(t)
	_, out := apiCreate(t, ts, "delete me", "")
	id := createdID(t, out)

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/pastes/"+id, nil)
	req.Header.Set("Authorization", "Bearer k1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp.StatusCode)
	}
	r2, _ := get(t, ts.URL+"/"+id, nil)
	if r2.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete: %d, want 404", r2.StatusCode)
	}
}

// ── view rendering ─────────────────────────────────────────────────────────

func TestHighlightKnownLangVsPlainFallback(t *testing.T) {
	_, ts := newTestServer(t)
	body := "package main\n\nfunc main() {}\n"

	_, out := apiCreate(t, ts, body, "?lang=go")
	resp, html := get(t, ts.URL+"/"+createdID(t, out), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(html, `<span class="kn"`) && !strings.Contains(html, `<span class="k"`) {
		t.Error("go paste has no keyword token spans — not highlighted")
	}

	_, out2 := apiCreate(t, ts, "just words, no language\n", "?lang=nosuchlang")
	resp2, html2 := get(t, ts.URL+"/"+createdID(t, out2), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("plain status = %d, want 200", resp2.StatusCode)
	}
	if strings.Contains(html2, `<span class="k"`) || strings.Contains(html2, `<span class="kn"`) {
		t.Error("unknown lang produced keyword spans — should be plain")
	}
	if !strings.Contains(html2, "just words, no language") {
		t.Error("plain body not rendered")
	}
}

func TestLineAnchorsPresent(t *testing.T) {
	_, ts := newTestServer(t)
	_, out := apiCreate(t, ts, "one\ntwo\nthree\n", "?lang=go")
	_, html := get(t, ts.URL+"/"+createdID(t, out), nil)
	for _, anchor := range []string{`id="L1"`, `id="L2"`, `id="L3"`} {
		if !strings.Contains(html, anchor) {
			t.Errorf("view HTML missing %s", anchor)
		}
	}
}

func TestRawIsExactBody(t *testing.T) {
	_, ts := newTestServer(t)
	body := "exact\n\tbytes & <tags> preserved\n"
	_, out := apiCreate(t, ts, body, "")
	resp, got := get(t, ts.URL+"/"+createdID(t, out)+"/raw", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type = %q, want text/plain", ct)
	}
	if got != body {
		t.Fatalf("raw = %q, want %q", got, body)
	}
}

func TestViewCountIncrements(t *testing.T) {
	s, ts := newTestServer(t)
	_, out := apiCreate(t, ts, "count me", "")
	id := createdID(t, out)
	get(t, ts.URL+"/"+id, nil)
	get(t, ts.URL+"/"+id, nil)
	p, err := getPaste(s.db, id)
	if err != nil || p == nil {
		t.Fatalf("getPaste: %v, %v", p, err)
	}
	if p.Views != 2 {
		t.Fatalf("views = %d, want 2", p.Views)
	}
}

// ── visibility ─────────────────────────────────────────────────────────────

func TestUnlistedAbsentFromPublicIndex(t *testing.T) {
	_, ts := newTestServer(t)
	apiCreate(t, ts, "public paste body", "?visibility=public&title=public-title")
	apiCreate(t, ts, "unlisted paste body", "?visibility=unlisted&title=unlisted-title")

	_, html := get(t, ts.URL+"/pastes", nil)
	if !strings.Contains(html, "public-title") {
		t.Error("public paste missing from /pastes")
	}
	if strings.Contains(html, "unlisted-title") {
		t.Error("unlisted paste leaked onto /pastes")
	}
}

func TestPrivateRequiresSession(t *testing.T) {
	_, ts := newTestServer(t)
	_, out := apiCreate(t, ts, "private body", "?visibility=private")
	id := createdID(t, out)

	// HTML view redirects to login.
	resp, _ := get(t, ts.URL+"/"+id, nil)
	if resp.StatusCode != http.StatusSeeOther ||
		resp.Header.Get("Location") != "/login" {
		t.Fatalf("html: status %d loc %q, want 303 /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}

	// Raw is a 401 JSON error.
	resp2, body := get(t, ts.URL+"/"+id+"/raw", nil)
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("raw: status %d, want 401", resp2.StatusCode)
	}
	if !strings.Contains(body, "error") {
		t.Errorf("raw 401 body = %q, want JSON error", body)
	}

	// The author session reads it fine.
	cookies := loginSession(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+"/"+id, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp3, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("authed get: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("authed: status %d, want 200", resp3.StatusCode)
	}
}

// ── view tokens ────────────────────────────────────────────────────────────

func TestTokenlessPasteViewsNormally(t *testing.T) {
	_, ts := newTestServer(t)
	_, out := apiCreate(t, ts, "open paste", "")
	resp, _ := get(t, ts.URL+"/"+createdID(t, out), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestLockedPageLeaksNothing(t *testing.T) {
	_, ts := newTestServer(t)
	body, title := "super secret contents", "secret-title"
	_, out := apiCreate(t, ts, body,
		"?token=opensesame&title="+title+"&lang=go")
	id := createdID(t, out)

	resp, html := get(t, ts.URL+"/"+id, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if strings.Contains(html, body) {
		t.Error("locked page leaks the body")
	}
	if strings.Contains(html, title) {
		t.Error("locked page leaks the title")
	}
	if strings.Contains(html, ">go<") {
		t.Error("locked page leaks the lang")
	}
	if !strings.Contains(strings.ToLower(html), "token") {
		t.Error("locked page does not present the unlock prompt")
	}

	// Raw without a token is 401 too.
	resp2, raw := get(t, ts.URL+"/"+id+"/raw", nil)
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("raw status = %d, want 401", resp2.StatusCode)
	}
	if strings.Contains(raw, body) {
		t.Error("locked raw leaks the body")
	}
}

func TestUnlockViaHeaderQueryAndForm(t *testing.T) {
	_, ts := newTestServer(t)
	body := "token gated text"
	_, out := apiCreate(t, ts, body, "?token=opensesame")
	id := createdID(t, out)

	// Header.
	resp, raw := get(t, ts.URL+"/"+id+"/raw",
		map[string]string{"X-Scrap-Token": "opensesame"})
	if resp.StatusCode != http.StatusOK || raw != body {
		t.Fatalf("header unlock: %d %q", resp.StatusCode, raw)
	}

	// Query.
	resp2, _ := get(t, ts.URL+"/"+id+"?t=opensesame", nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("query unlock: %d, want 200", resp2.StatusCode)
	}

	// Form unlock sets a per-paste cookie and redirects to the view.
	resp3, err := noRedirectClient().PostForm(ts.URL+"/"+id+"/unlock",
		url.Values{"token": {"opensesame"}})
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusSeeOther ||
		resp3.Header.Get("Location") != "/"+id {
		t.Fatalf("unlock: status %d loc %q", resp3.StatusCode,
			resp3.Header.Get("Location"))
	}
	var unlock *http.Cookie
	for _, c := range resp3.Cookies() {
		if c.Name == "scrap_t" {
			unlock = c
		}
	}
	if unlock == nil {
		t.Fatal("unlock did not set the scrap_t cookie")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/"+id, nil)
	req.AddCookie(unlock)
	resp4, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("cookie view: %v", err)
	}
	defer resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK {
		t.Fatalf("cookie view: %d, want 200", resp4.StatusCode)
	}
	b, _ := io.ReadAll(resp4.Body)
	if !strings.Contains(string(b), "token gated text") {
		t.Error("unlocked view does not render the body")
	}
}

func TestWrongTokenIs403ThenRateLimited(t *testing.T) {
	_, ts := newTestServer(t)
	_, out := apiCreate(t, ts, "rate limited paste", "?token=opensesame")
	id := createdID(t, out)

	for i := 0; i < 5; i++ {
		resp, _ := get(t, ts.URL+"/"+id+"/raw",
			map[string]string{"X-Scrap-Token": fmt.Sprintf("wrong-%d", i)})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("attempt %d: status %d, want 403", i+1, resp.StatusCode)
		}
	}
	// The budget is spent — even a wrong attempt now answers 429.
	resp, _ := get(t, ts.URL+"/"+id+"/raw",
		map[string]string{"X-Scrap-Token": "wrong-again"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
}

func TestTokenForcesUnlisted(t *testing.T) {
	s, ts := newTestServer(t)
	_, out := apiCreate(t, ts, "wants to be public",
		"?visibility=public&token=opensesame&title=locked-public")
	id := createdID(t, out)

	p, err := getPaste(s.db, id)
	if err != nil || p == nil {
		t.Fatalf("getPaste: %v, %v", p, err)
	}
	if p.Visibility != VisUnlisted {
		t.Fatalf("visibility = %q, want unlisted (token-forced)", p.Visibility)
	}
	_, html := get(t, ts.URL+"/pastes", nil)
	if strings.Contains(html, "locked-public") {
		t.Error("token-locked paste leaked onto the public index")
	}
}

// ── expiry ─────────────────────────────────────────────────────────────────

func TestExpiredIs410AndRowDeleted(t *testing.T) {
	s, ts := newTestServer(t)
	_, out := apiCreate(t, ts, "short lived", "?expires=1h")
	id := createdID(t, out)

	// Force the deadline into the past.
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := s.db.Exec(
		`UPDATE pastes SET expires_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	resp, html := get(t, ts.URL+"/"+id, nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want 410", resp.StatusCode)
	}
	if !strings.Contains(html, "expired") {
		t.Error("410 page is not the distinct expired copy")
	}
	// The row is gone — lazily deleted on read.
	p, err := getPaste(s.db, id)
	if err != nil {
		t.Fatalf("getPaste: %v", err)
	}
	if p != nil {
		t.Error("expired row still present after read")
	}
	// A never-existed id is a plain 404, distinct from 410.
	resp2, _ := get(t, ts.URL+"/b234567abcdefghi", nil)
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("missing paste status = %d, want 404", resp2.StatusCode)
	}
}

func TestExpirySweep(t *testing.T) {
	s, ts := newTestServer(t)
	_, out := apiCreate(t, ts, "sweep me", "?expires=1h&token=tok")
	id := createdID(t, out)
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := s.db.Exec(
		`UPDATE pastes SET expires_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	n, err := deleteExpiredPastes(s.db)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	var tokens int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM tokens WHERE paste_id = ?`, id).Scan(&tokens); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokens != 0 {
		t.Error("sweep left orphaned tokens")
	}
}

// ── manage ─────────────────────────────────────────────────────────────────

func authedGet(t *testing.T, ts *httptest.Server, cookies []*http.Cookie, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestManageListsFiltersAndDeletes(t *testing.T) {
	_, ts := newTestServer(t)
	cookies := loginSession(t, ts)

	_, out1 := apiCreate(t, ts, "alpha go body", "?lang=go&title=alpha&visibility=public")
	apiCreate(t, ts, "bravo py body", "?lang=python&title=bravo&visibility=private")
	id1 := createdID(t, out1)

	// Unauthenticated manage redirects to login.
	resp, _ := get(t, ts.URL+"/manage", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauth manage: %d, want 303", resp.StatusCode)
	}

	// Lists everything.
	code, html := authedGet(t, ts, cookies, "/manage")
	if code != http.StatusOK {
		t.Fatalf("manage: %d, want 200", code)
	}
	if !strings.Contains(html, "alpha") || !strings.Contains(html, "bravo") {
		t.Error("manage does not list all pastes")
	}

	// Lang filter.
	_, html = authedGet(t, ts, cookies, "/manage?lang=go")
	if !strings.Contains(html, "alpha") || strings.Contains(html, ">bravo<") {
		t.Error("lang filter wrong")
	}

	// Visibility filter.
	_, html = authedGet(t, ts, cookies, "/manage?visibility=private")
	if strings.Contains(html, ">alpha<") || !strings.Contains(html, "bravo") {
		t.Error("visibility filter wrong")
	}

	// Search hits the body text.
	_, html = authedGet(t, ts, cookies, "/manage?q=py+body")
	if strings.Contains(html, ">alpha<") || !strings.Contains(html, "bravo") {
		t.Error("search filter wrong")
	}

	// Per-row delete.
	req, _ := http.NewRequest("POST", ts.URL+"/pastes/"+id1+"/delete", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp2, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d, want 303", resp2.StatusCode)
	}
	_, html = authedGet(t, ts, cookies, "/manage")
	if strings.Contains(html, ">alpha<") {
		t.Error("deleted paste still listed")
	}
}

func TestManageDeleteExpiredBulk(t *testing.T) {
	s, ts := newTestServer(t)
	cookies := loginSession(t, ts)
	_, out := apiCreate(t, ts, "stale", "?expires=1h&title=stale-one")
	apiCreate(t, ts, "fresh", "?title=fresh-one")
	id := createdID(t, out)
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := s.db.Exec(
		`UPDATE pastes SET expires_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	req, _ := http.NewRequest("POST", ts.URL+"/pastes/expired/delete", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("bulk delete: %d, want 303", resp.StatusCode)
	}
	_, html := authedGet(t, ts, cookies, "/manage")
	if strings.Contains(html, "stale-one") {
		t.Error("expired paste survived the bulk delete")
	}
	if !strings.Contains(html, "fresh-one") {
		t.Error("unexpired paste was deleted by the bulk delete")
	}
}

// ── misc ───────────────────────────────────────────────────────────────────

func TestStatus(t *testing.T) {
	_, ts := newTestServer(t)
	apiCreate(t, ts, "one paste", "")
	resp, body := get(t, ts.URL+"/status", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var v struct {
		Service string `json:"service"`
		OK      bool   `json:"ok"`
		Pastes  int    `json:"pastes"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Service != "scrap" || !v.OK || v.Pastes != 1 {
		t.Fatalf("status = %+v", v)
	}
}

func TestReservedAndMalformedIDsAre404(t *testing.T) {
	_, ts := newTestServer(t)
	for _, path := range []string{"/favicon.ico", "/ABCDEFGHIJKLMNOP", "/short1"} {
		resp, _ := get(t, ts.URL+path, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestInvalidExpiryRejected(t *testing.T) {
	_, ts := newTestServer(t)
	status, _ := apiCreate(t, ts, "body", "?expires=2years")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}
