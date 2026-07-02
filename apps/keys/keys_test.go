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
	tmpl, err := web.ParseTemplates(assets, tmplFuncs)
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	return &Server{
		db:    db,
		ks:    ks,
		auth:  &web.Auth{DB: db, Password: "secret"},
		rd:    &web.Renderer{Templates: tmpl, AssetVer: "test"},
		logrl: web.NewFailLimiter(5, time.Minute),
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
