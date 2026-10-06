package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/keys"
)

// The verifier is fixed so the challenge is too; any 43–128 unreserved
// characters will do.
const testVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk-desktop"

func challengeFor(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

const testRedirect = "http://127.0.0.1:53682/callback"

func authorizeParams() url.Values {
	return url.Values{
		"client_id":             {deviceClientID},
		"redirect_uri":          {testRedirect},
		"code_challenge":        {challengeFor(testVerifier)},
		"code_challenge_method": {"S256"},
		"state":                 {"xyz-123"},
		"device":                {"MacBook"},
	}
}

func get(t *testing.T, ts *httptest.Server, cookies []*http.Cookie, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// approve walks GET → Allow and returns the code from the loopback redirect.
func approve(t *testing.T, ts *httptest.Server, cookies []*http.Cookie) string {
	t.Helper()
	resp := postForm(t, ts, cookies, "/device/authorize", withAction(authorizeParams(), "allow"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("allow status = %d, want 303", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != testRedirect {
		t.Fatalf("allow redirected to %q, want the loopback URI", resp.Header.Get("Location"))
	}
	if loc.Query().Get("state") != "xyz-123" {
		t.Errorf("state = %q, want it echoed", loc.Query().Get("state"))
	}
	code := loc.Query().Get("code")
	if len(code) != 43 {
		t.Fatalf("code = %q, want 32 random bytes in base64url", code)
	}
	return code
}

func withAction(v url.Values, action string) url.Values {
	v.Set("action", action)
	return v
}

type tokenResp struct {
	status int
	body   map[string]string
	hdr    http.Header
}

func redeem(t *testing.T, ts *httptest.Server, form url.Values, headers map[string]string) tokenResp {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/device/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := tokenResp{status: resp.StatusCode, hdr: resp.Header, body: map[string]string{}}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func tokenForm(code, verifier string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {testRedirect},
	}
}

func TestDeviceAuthorizeNeedsSession(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	path := "/device/authorize?" + authorizeParams().Encode()
	resp := get(t, ts, nil, path)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 to login", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Path != "/login" || loc.Query().Get("next") != path {
		t.Errorf("Location = %q, want /login?next=<this URL>", resp.Header.Get("Location"))
	}

	// Signed in, the same URL is the consent page naming the device.
	resp = get(t, ts, loginSession(t, ts), path)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Sign in Farfield on MacBook?") {
		t.Errorf("consent page: %d %s", resp.StatusCode, body)
	}

	// And a sign-in carrying that next comes straight back to it.
	pw := postForm(t, ts, nil, "/login", url.Values{"password": {"secret"}, "next": {path}})
	pw.Body.Close()
	if got := pw.Header.Get("Location"); got != path {
		t.Errorf("login with next redirected to %q, want %q", got, path)
	}
}

func TestDeviceAuthorizeRejectsBadRequests(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	cookies := loginSession(t, ts)

	for name, mutate := range map[string]func(url.Values){
		"https redirect":     func(v url.Values) { v.Set("redirect_uri", "https://127.0.0.1:5000/cb") },
		"remote host":        func(v url.Values) { v.Set("redirect_uri", "http://evil.example:5000/cb") },
		"loopback lookalike": func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1.evil.example:5000/cb") },
		"userinfo":           func(v url.Values) { v.Set("redirect_uri", "http://x@127.0.0.1:5000/cb") },
		"no port":            func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1/cb") },
		"fragment":           func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:5000/cb#x") },
		"other 127/8":        func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.2:5000/cb") },
		"unknown client":     func(v url.Values) { v.Set("client_id", "someone-else") },
		"plain pkce":         func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"short challenge":    func(v url.Values) { v.Set("code_challenge", "abc") },
	} {
		v := authorizeParams()
		mutate(v)
		for _, c := range [][]*http.Cookie{nil, cookies} {
			resp := get(t, ts, c, "/device/authorize?"+v.Encode())
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
				t.Errorf("%s (session %v): %d Location %q; want 400, no redirect",
					name, c != nil, resp.StatusCode, resp.Header.Get("Location"))
			}
		}
		// The POST re-validates: hidden fields come back from the browser.
		resp := postForm(t, ts, cookies, "/device/authorize", withAction(v, "allow"))
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
			t.Errorf("%s POST: %d Location %q; want 400, no redirect",
				name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	for _, ok := range []string{"http://[::1]:8080/", "http://localhost:1/x/y?keep=1", "http://127.0.0.1:65535"} {
		if _, valid := loopbackRedirect(ok); !valid {
			t.Errorf("loopbackRedirect(%q) refused a loopback URI", ok)
		}
	}
}

func TestDeviceDeny(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	resp := postForm(t, ts, loginSession(t, ts), "/device/authorize", withAction(authorizeParams(), "deny"))
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Query().Get("error") != "access_denied" ||
		loc.Query().Get("state") != "xyz-123" || loc.Query().Get("code") != "" {
		t.Fatalf("deny: %d %q; want 303 with error=access_denied and state", resp.StatusCode, loc)
	}
}

// The POST needs a session too: an anonymous form post must not mint a code.
func TestDeviceDecideNeedsSession(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	resp := postForm(t, ts, nil, "/device/authorize", withAction(authorizeParams(), "allow"))
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "code=") || !strings.HasPrefix(loc, "/login") {
		t.Fatalf("anonymous allow redirected to %q, want login", loc)
	}
}

func TestDeviceTokenFlow(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	cookies := loginSession(t, ts)

	// A wrong verifier fails — and burns the code, so the right one is too late.
	code := approve(t, ts, cookies)
	wrong := strings.Repeat("A", 43)
	if r := redeem(t, ts, tokenForm(code, wrong), nil); r.status != 400 || r.body["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %v; want 400 invalid_grant", r.status, r.body)
	}
	if r := redeem(t, ts, tokenForm(code, testVerifier), nil); r.status != 400 {
		t.Fatalf("code after a failed attempt: %d, want 400 (single use)", r.status)
	}

	// A mismatched redirect_uri also fails.
	code = approve(t, ts, cookies)
	f := tokenForm(code, testVerifier)
	f.Set("redirect_uri", "http://127.0.0.1:1/other")
	if r := redeem(t, ts, f, nil); r.status != 400 {
		t.Fatalf("other redirect_uri: %d, want 400", r.status)
	}

	// The right verifier gets a key that opens the fleet.
	code = approve(t, ts, cookies)
	r := redeem(t, ts, tokenForm(code, testVerifier), nil)
	if r.status != 200 {
		t.Fatalf("redeem: %d %v", r.status, r.body)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", r.hdr.Get("Cache-Control"))
	}
	tok := r.body["access_token"]
	if !strings.HasPrefix(tok, "ffk_") || r.body["token_type"] != "api-key" ||
		r.body["app"] != "*" || r.body["scope"] != "write" || r.body["key_id"] == "" {
		t.Fatalf("token response = %v", r.body)
	}
	if scope, ok := s.ks.CheckRequest(tok, "content", "GET", "/api/entries"); !ok || scope != keys.ScopeWrite {
		t.Errorf("minted key on content = %q, %v; want write", scope, ok)
	}
	k, _ := s.ks.Get(r.body["key_id"])
	if k == nil || k.Name != "Farfield on MacBook" || k.ExpiresAt != "" {
		t.Errorf("minted key record = %+v", k)
	}

	// Redeeming the same code twice fails.
	if r := redeem(t, ts, tokenForm(code, testVerifier), nil); r.status != 400 {
		t.Fatalf("second redemption: %d, want 400", r.status)
	}

	// JSON bodies work too.
	code = approve(t, ts, cookies)
	b, _ := json.Marshal(map[string]string{"grant_type": "authorization_code", "code": code,
		"code_verifier": testVerifier, "redirect_uri": testRedirect})
	resp, err := http.Post(ts.URL+"/device/token", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("JSON redeem: %d, want 200", resp.StatusCode)
	}
}

func TestDeviceTokenExpired(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	code := approve(t, ts, loginSession(t, ts))
	// Under the store's lock: a kept-alive connection's handler may already be live.
	s.grants.mu.Lock()
	s.grants.now = func() time.Time { return time.Now().Add(grantTTL + time.Second) }
	s.grants.mu.Unlock()
	if r := redeem(t, ts, tokenForm(code, testVerifier), nil); r.status != 400 || r.body["error"] != "invalid_grant" {
		t.Fatalf("expired grant: %d %v; want 400 invalid_grant", r.status, r.body)
	}
}

func TestDeviceTokenPrivateIngressOnly(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	code := approve(t, ts, loginSession(t, ts))
	if r := redeem(t, ts, tokenForm(code, testVerifier), map[string]string{"Cf-Ray": "8abc-SJC"}); r.status != 404 {
		t.Fatalf("via tunnel: %d, want 404", r.status)
	}
	// The tunnel's 404 never reached the grant, so the code still works.
	if r := redeem(t, ts, tokenForm(code, testVerifier), nil); r.status != 200 {
		t.Fatalf("private redeem after a tunnel probe: %d, want 200", r.status)
	}
}

func TestDeviceTokenThrottlesGuessing(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	for range 10 {
		redeem(t, ts, tokenForm("nope", testVerifier), nil)
	}
	if r := redeem(t, ts, tokenForm("nope", testVerifier), nil); r.status != http.StatusTooManyRequests {
		t.Fatalf("after 10 failures: %d, want 429", r.status)
	}
}

// Signed in, the login page forwards to a same-host next — but not to another
// app's URL: that app sent the browser here because it cannot see this
// session, and forwarding back would loop forever.
func TestLoginPageForwardsSameHostOnly(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	cookies := loginSession(t, ts)

	resp := get(t, ts, cookies, "/login?next=%2Fnew")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/new" {
		t.Errorf("same-host next: %d %q; want 303 /new", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp = get(t, ts, cookies, "/login?next="+url.QueryEscape("http://localhost:8787/entries"))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `href="http://localhost:8787/login"`) {
		t.Errorf("cross-host next while signed in: %d; want the page with a link to that app's login\n%s",
			resp.StatusCode, body)
	}
}
