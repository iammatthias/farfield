package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/keys"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

// ── helpers ────────────────────────────────────────────────────────────────

func newTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.OpenDB(filepath.Join(t.TempDir(), "keys.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(store.SessionSchema); err != nil {
		t.Fatalf("session schema: %v", err)
	}
	ks, err := keys.New(db)
	if err != nil {
		t.Fatalf("keys.New: %v", err)
	}
	t.Cleanup(func() { ks.Close() })
	tmpl, err := web.ParseTemplates(assets, tmplFuncs)
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	return &Server{
		db:   db,
		ks:   ks,
		auth: &web.Auth{DB: db, Password: "secret"},
		rd:   &web.Renderer{Templates: tmpl, AssetVer: "test"},
	}
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
	resp, err := noRedirectClient().PostForm(ts.URL+"/login", url.Values{"password": {"secret"}})
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

func postForm(t *testing.T, ts *httptest.Server, cookies []*http.Cookie, path string, form url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

var tokenRe = regexp.MustCompile(`ffk_[a-z0-9]+`)

// revealRe finds the one-time token reveal itself — a rotate page also shows
// the replaced key's hint, which tokenRe alone would match first.
var revealRe = regexp.MustCompile(`<code class="token">(ffk_[a-z0-9]+)</code>`)

// ── tests ──────────────────────────────────────────────────────────────────

func TestMintRevokeRoundTrip(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	cookies := loginSession(t, ts)

	// Mint through the form; the created page reveals the token once.
	resp := postForm(t, ts, cookies, "/keys", url.Values{
		"name": {"intern"}, "app": {"library"}, "scope": {"upload"},
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create key status = %d: %s", resp.StatusCode, body)
	}
	token := tokenRe.FindString(string(body))
	if token == "" {
		t.Fatalf("created page does not reveal a token: %s", body)
	}

	// The minted key resolves through the shared store for its app + scope.
	if scope, ok := s.ks.Check(token, "library"); !ok || scope != "upload" {
		t.Fatalf("Check = %q, %v; want upload, true", scope, ok)
	}
	if _, ok := s.ks.Check(token, "feed"); ok {
		t.Error("library key accepted for feed")
	}

	// Revoke through the UI; the key dies immediately.
	ks, _ := s.ks.List()
	if len(ks) != 1 {
		t.Fatalf("List = %d keys, want 1", len(ks))
	}
	resp = postForm(t, ts, cookies, "/keys/"+ks[0].ID+"/revoke", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke status = %d, want 303", resp.StatusCode)
	}
	if _, ok := s.ks.Check(token, "library"); ok {
		t.Error("revoked key still resolves")
	}
}

func TestAdminRequiresSession(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := noRedirectClient().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("anonymous index = %d, want 303 to login", resp.StatusCode)
	}

	resp = postForm(t, ts, nil, "/keys", url.Values{
		"name": {"x"}, "app": {"feed"}, "scope": {"write"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("anonymous mint = %d, want 303 to login", resp.StatusCode)
	}
	if ks, _ := s.ks.List(); len(ks) != 0 {
		t.Error("anonymous mint created a key")
	}
}

func TestLoginFailureLimited(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	c := noRedirectClient()
	for i := 0; i < 5; i++ {
		resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {"wrong"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("failed login %d = %d, want 303 back to form", i, resp.StatusCode)
		}
	}
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("6th attempt = %d, want 429 (blocked even with the right password)", resp.StatusCode)
	}
}

func TestStatus(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func getPage(t *testing.T, ts *httptest.Server, cookies []*http.Cookie, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestKeyPage(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	cookies := loginSession(t, ts)

	token, k, err := s.ks.Mint("intern uploads", "library", keys.ScopeUpload, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	s.ks.CheckRequest(token, "library", "POST", "/api/books")
	s.ks.CheckRequest(token, "feed", "GET", "/api/posts")
	if err := s.ks.Flush(); err != nil {
		t.Fatal(err)
	}

	code, body := getPage(t, ts, cookies, "/keys/"+k.ID)
	if code != http.StatusOK {
		t.Fatalf("key page = %d: %s", code, body)
	}
	for _, want := range []string{"intern uploads", k.Hint + "…", "/api/books", "wrong-app",
		"/keys/" + k.ID + "/rotate", "/keys/" + k.ID + "/revoke"} {
		if !strings.Contains(body, want) {
			t.Errorf("key page lacks %q", want)
		}
	}
	if strings.Contains(body, token) {
		t.Error("key page reveals the token")
	}

	if code, _ := getPage(t, ts, cookies, "/keys/nope"); code != http.StatusNotFound {
		t.Errorf("unknown key page = %d, want 404", code)
	}
	if code, _ := getPage(t, ts, nil, "/keys/"+k.ID); code != http.StatusSeeOther {
		t.Errorf("anonymous key page = %d, want 303 to login", code)
	}

	// The index links each key to its page.
	if _, body := getPage(t, ts, cookies, "/"); !strings.Contains(body, `href="/keys/`+k.ID+`"`) {
		t.Error("index does not link to the key page")
	}
	// So does the ⌘K menu.
	found := false
	for _, it := range s.paletteItems(httptest.NewRequest("GET", "/", nil)) {
		if it.URL == "/keys/"+k.ID {
			found = true
		}
	}
	if !found {
		t.Error("palette record does not open the key page")
	}
}

func TestKeyPageControls(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	cookies := loginSession(t, ts)
	_, k, _ := s.ks.Mint("old name", "feed", keys.ScopeRead, time.Time{})

	resp := postForm(t, ts, cookies, "/keys/"+k.ID+"/rename", url.Values{"name": {"new name"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/keys/"+k.ID {
		t.Errorf("rename = %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp = postForm(t, ts, cookies, "/keys/"+k.ID+"/expiry", url.Values{
		"expires": {time.Now().AddDate(0, 0, 10).Format("2006-01-02")}, "action": {"set"}})
	resp.Body.Close()
	got, _ := s.ks.Get(k.ID)
	if got.Name != "new name" || got.ExpiresAt == "" || !got.Active() {
		t.Errorf("after rename + expiry: %+v", got)
	}
	resp = postForm(t, ts, cookies, "/keys/"+k.ID+"/expiry", url.Values{
		"expires": {"2001-01-01"}, "action": {"set"}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "today or later") {
		t.Error("a past expiry was not refused")
	}
	resp = postForm(t, ts, cookies, "/keys/"+k.ID+"/expiry", url.Values{"action": {"clear"}})
	resp.Body.Close()
	if got, _ := s.ks.Get(k.ID); got.ExpiresAt != "" {
		t.Error("expiry not cleared")
	}

	// Revoke from the key page lands back on it.
	resp = postForm(t, ts, cookies, "/keys/"+k.ID+"/revoke", url.Values{"from": {"key"}})
	resp.Body.Close()
	if resp.Header.Get("Location") != "/keys/"+k.ID {
		t.Errorf("revoke from key page → %q", resp.Header.Get("Location"))
	}
	resp = postForm(t, ts, cookies, "/keys/"+k.ID+"/delete", nil)
	resp.Body.Close()
	if code, _ := getPage(t, ts, cookies, "/keys/"+k.ID); code != http.StatusNotFound {
		t.Errorf("deleted key page = %d, want 404", code)
	}
}

func TestRotateFlow(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	cookies := loginSession(t, ts)
	oldToken, k, _ := s.ks.Mint("ci", "blobs", keys.ScopeWrite, time.Time{})

	resp := postForm(t, ts, cookies, "/keys/"+k.ID+"/rotate", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate = %d: %s", resp.StatusCode, body)
	}
	var token string
	if m := revealRe.FindStringSubmatch(string(body)); m != nil {
		token = m[1]
	}
	if token == "" || token == oldToken {
		t.Fatalf("rotate did not reveal a new token")
	}
	if scope, ok := s.ks.Check(token, "blobs"); !ok || scope != keys.ScopeWrite {
		t.Error("rotated token does not check as blobs/write")
	}
	if _, ok := s.ks.Check(oldToken, "blobs"); ok {
		t.Error("old token still valid after rotate")
	}
	if !strings.Contains(string(body), "Key rotated") {
		t.Error("rotate page does not say it replaced a key")
	}

	// Rotating the now-revoked key is refused, not a second mint.
	resp = postForm(t, ts, cookies, "/keys/"+k.ID+"/rotate", nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if revealRe.MatchString(string(body)) {
		t.Error("rotating a revoked key minted a token")
	}
	if ks, _ := s.ks.List(); len(ks) != 2 {
		t.Errorf("keys after rotate = %d, want 2", len(ks))
	}
}
