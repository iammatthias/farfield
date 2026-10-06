package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/descope/virtualwebauthn"
)

// deviceAssertion begins a passkey approval of the test authorize request and
// returns the authenticator's answer.
func deviceAssertion(t *testing.T, c *http.Client, base string, a *virtualwebauthn.Authenticator) string {
	t.Helper()
	params := map[string]string{}
	for k, v := range authorizeParams() {
		params[k] = v[0]
	}
	body, _ := json.Marshal(params)
	resp, opts := do(t, c, "POST", base+"/passkey/device/begin", "application/json", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device/begin: %d %s", resp.StatusCode, opts)
	}
	parsed, err := virtualwebauthn.ParseAssertionOptions(string(opts))
	if err != nil {
		t.Fatalf("parse assertion options: %v", err)
	}
	cred := a.FindAllowedCredential(*parsed)
	if cred == nil {
		t.Fatal("no allowed credential on the authenticator")
	}
	return virtualwebauthn.CreateAssertionResponse(testRP, *a, *cred, *parsed)
}

func authorizePath() string { return "/device/authorize?" + authorizeParams().Encode() }

// With passkeys on, a browser session never approves a device; creating the
// first passkey approves nothing; only a passkey assertion issues a code.
func TestDeviceApprovalNeedsPasskey(t *testing.T) {
	s, ts := passkeyServer(t)
	authr := virtualwebauthn.NewAuthenticator()

	// No session, no passkey: sign in first, and come back here.
	anon := browser(t)
	resp, _ := do(t, anon, "GET", ts.URL+authorizePath(), "", nil)
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/login?next=") {
		t.Fatalf("anonymous authorize: %d %q, want 303 to /login", resp.StatusCode, loc)
	}

	// Signed in: the page offers to create a passkey — and no Allow.
	owner := browser(t)
	passwordLogin(t, owner, ts)
	_, page := do(t, owner, "GET", ts.URL+authorizePath(), "", nil)
	if !bytes.Contains(page, []byte("data-device-register")) || bytes.Contains(page, []byte(`value="allow"`)) {
		t.Fatalf("first-time page should offer only passkey creation: %s", page)
	}
	// The session's Allow is refused outright.
	cookies := owner.Jar.Cookies(mustURL(t, ts.URL))
	resp = postForm(t, ts, cookies, "/device/authorize", withAction(authorizeParams(), "allow"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("session Allow with passkeys on: %d, want 400", resp.StatusCode)
	}

	// Creating the passkey issues no code.
	register(t, owner, ts, &authr, "MacBook")
	if n := s.grants.len(); n != 0 {
		t.Fatalf("grants after creating a passkey = %d, want 0", n)
	}
	_, page = do(t, owner, "GET", ts.URL+authorizePath()+"&created=1", "", nil)
	if !bytes.Contains(page, []byte("data-device-approve")) || !bytes.Contains(page, []byte("Passkey created")) {
		t.Fatalf("after creation the page should ask for the passkey: %s", page)
	}

	// The passkey approves — from a browser with no session at all.
	answer := deviceAssertion(t, anon, ts.URL, &authr)
	resp, out := do(t, anon, "POST", ts.URL+"/passkey/device/finish", "application/json", []byte(answer))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device/finish: %d %s", resp.StatusCode, out)
	}
	for _, c := range resp.Cookies() {
		if c.MaxAge >= 0 && c.Name != ceremonyCookie {
			t.Errorf("approval set cookie %q; it must not sign the browser in", c.Name)
		}
	}
	var fin struct{ Redirect string }
	_ = json.Unmarshal(out, &fin)
	loc, err := url.Parse(fin.Redirect)
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != testRedirect || loc.Query().Get("state") != "xyz-123" {
		t.Fatalf("redirect = %q, want the loopback URI with state", fin.Redirect)
	}
	tok := redeem(t, ts, tokenForm(loc.Query().Get("code"), testVerifier), nil)
	if tok.status != http.StatusOK || !strings.HasPrefix(tok.body["access_token"], "ffk_") {
		t.Fatalf("redeem: %d %v", tok.status, tok.body)
	}

	// A replayed answer approves nothing.
	if resp, _ := do(t, browser(t), "POST", ts.URL+"/passkey/device/finish", "application/json", []byte(answer)); resp.StatusCode == http.StatusOK {
		t.Error("a replayed assertion approved a device")
	}
}

// Signing a device in again replaces its key instead of adding another.
func TestDeviceSignInReplacesItsKey(t *testing.T) {
	s, ts := passkeyServer(t)
	authr := virtualwebauthn.NewAuthenticator()
	owner := browser(t)
	passwordLogin(t, owner, ts)
	register(t, owner, ts, &authr, "MacBook")

	signIn := func() string {
		c := browser(t)
		answer := deviceAssertion(t, c, ts.URL, &authr)
		_, out := do(t, c, "POST", ts.URL+"/passkey/device/finish", "application/json", []byte(answer))
		var fin struct{ Redirect string }
		_ = json.Unmarshal(out, &fin)
		loc, _ := url.Parse(fin.Redirect)
		tok := redeem(t, ts, tokenForm(loc.Query().Get("code"), testVerifier), nil)
		return tok.body["key_id"]
	}
	first, second := signIn(), signIn()
	k1, _ := s.ks.Get(first)
	k2, _ := s.ks.Get(second)
	if k1 == nil || k2 == nil || k1.Active() || !k2.Active() {
		t.Fatalf("after a second sign-in the first key should be revoked and the second live: %+v %+v", k1, k2)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
