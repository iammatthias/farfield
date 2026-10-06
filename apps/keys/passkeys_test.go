package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
	"github.com/go-webauthn/webauthn/webauthn"
)

// The relying party the tests run as. The origin is data inside the signed
// client data, so it need not be the httptest server's address.
var testRP = virtualwebauthn.RelyingParty{Name: "farfield", ID: "localhost", Origin: "http://localhost:8801"}

func passkeyServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	wa, err := webauthn.New(&webauthn.Config{
		RPID: testRP.ID, RPDisplayName: testRP.Name, RPOrigins: []string{testRP.Origin},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServerWA(t, wa)
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

// browser is a cookie-keeping client that does not follow redirects, so a
// test sees each 303 — the ceremony cookie is path-scoped and must round-trip.
func browser(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func do(t *testing.T, c *http.Client, method, u, ctype string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, u, bytes.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func passwordLogin(t *testing.T, c *http.Client, ts *httptest.Server) {
	t.Helper()
	resp, _ := do(t, c, "POST", ts.URL+"/login", "application/x-www-form-urlencoded",
		[]byte(url.Values{"password": {"secret"}}.Encode()))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("password login: %d", resp.StatusCode)
	}
}

// register runs the add-passkey ceremony through the real handlers.
func register(t *testing.T, c *http.Client, ts *httptest.Server, a *virtualwebauthn.Authenticator, name string) virtualwebauthn.Credential {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	resp, opts := do(t, c, "POST", ts.URL+"/passkey/register/begin", "application/json", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register/begin: %d %s", resp.StatusCode, opts)
	}
	parsed, err := virtualwebauthn.ParseAttestationOptions(string(opts))
	if err != nil {
		t.Fatalf("parse attestation options: %v (%s)", err, opts)
	}
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	att := virtualwebauthn.CreateAttestationResponse(testRP, *a, cred, *parsed)
	resp, out := do(t, c, "POST", ts.URL+"/passkey/register/finish", "application/json", []byte(att))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register/finish: %d %s", resp.StatusCode, out)
	}
	a.AddCredential(cred)
	return cred
}

// assertion begins a passkey login and returns the authenticator's answer.
func assertion(t *testing.T, c *http.Client, ts *httptest.Server, a *virtualwebauthn.Authenticator, next string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"next": next})
	resp, opts := do(t, c, "POST", ts.URL+"/passkey/login/begin", "application/json", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login/begin: %d %s", resp.StatusCode, opts)
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

func TestPasskeyRegisterAndLogin(t *testing.T) {
	_, ts := passkeyServer(t)
	authr := virtualwebauthn.NewAuthenticator()

	// Bootstrap: no passkeys, so the login page is the password form alone.
	anon := browser(t)
	if _, page := do(t, anon, "GET", ts.URL+"/login", "", nil); bytes.Contains(page, []byte("data-passkey-login")) {
		t.Error("login page offers a passkey before one exists")
	}

	owner := browser(t)
	passwordLogin(t, owner, ts)
	register(t, owner, ts, &authr, "MacBook")
	if _, page := do(t, owner, "GET", ts.URL+"/passkeys", "", nil); !bytes.Contains(page, []byte("MacBook")) {
		t.Fatalf("passkeys page does not list the new passkey: %s", page)
	}

	// A fresh browser signs in with the passkey alone.
	if _, page := do(t, anon, "GET", ts.URL+"/login?next=%2Fnew", "", nil); !bytes.Contains(page, []byte("data-passkey-login")) {
		t.Fatal("login page does not offer the passkey")
	}
	resp, _ := do(t, anon, "GET", ts.URL+"/", "", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("console before passkey login: %d, want 303", resp.StatusCode)
	}
	answer := assertion(t, anon, ts, &authr, "/new")
	resp, out := do(t, anon, "POST", ts.URL+"/passkey/login/finish", "application/json", []byte(answer))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login/finish: %d %s", resp.StatusCode, out)
	}
	var fin struct{ Next string }
	_ = json.Unmarshal(out, &fin)
	if fin.Next != "/new" {
		t.Errorf("next = %q, want /new", fin.Next)
	}
	if resp, _ := do(t, anon, "GET", ts.URL+"/", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("console after passkey login: %d, want 200", resp.StatusCode)
	}

	// The ceremony was single use: the same answer cannot sign in again.
	replay := browser(t)
	if resp, _ := do(t, replay, "POST", ts.URL+"/passkey/login/finish", "application/json", []byte(answer)); resp.StatusCode == http.StatusOK {
		t.Error("a replayed assertion signed in")
	}
	// Nor can an answer to another ceremony's challenge.
	stale := assertion(t, replay, ts, &authr, "")
	_ = assertion(t, replay, ts, &authr, "") // a newer ceremony replaces the cookie
	if resp, _ := do(t, replay, "POST", ts.URL+"/passkey/login/finish", "application/json", []byte(stale)); resp.StatusCode == http.StatusOK {
		t.Error("an assertion for a superseded challenge signed in")
	}

	// A foreign next is dropped, not followed.
	other := browser(t)
	answer = assertion(t, other, ts, &authr, "https://evil.example/")
	_, out = do(t, other, "POST", ts.URL+"/passkey/login/finish", "application/json", []byte(answer))
	_ = json.Unmarshal(out, &fin)
	if fin.Next != "/" {
		t.Errorf("foreign next came back as %q, want /", fin.Next)
	}
}

// A passkey login spends the same per-client budget as a wrong password.
func TestPasskeyLoginFailuresThrottle(t *testing.T) {
	_, ts := passkeyServer(t)
	authr := virtualwebauthn.NewAuthenticator()
	owner := browser(t)
	passwordLogin(t, owner, ts)
	register(t, owner, ts, &authr, "MacBook")

	// An authenticator holding a key the server never saw.
	rogue := virtualwebauthn.NewAuthenticator()
	c := browser(t)
	for i := range 5 {
		body, _ := json.Marshal(map[string]string{})
		_, opts := do(t, c, "POST", ts.URL+"/passkey/login/begin", "application/json", body)
		parsed, err := virtualwebauthn.ParseAssertionOptions(string(opts))
		if err != nil {
			t.Fatalf("attempt %d: %v (%s)", i+1, err, opts)
		}
		fake := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
		ans := virtualwebauthn.CreateAssertionResponse(testRP, rogue, fake, *parsed)
		if resp, _ := do(t, c, "POST", ts.URL+"/passkey/login/finish", "application/json", []byte(ans)); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i+1, resp.StatusCode)
		}
	}
	if resp, _ := do(t, c, "POST", ts.URL+"/passkey/login/begin", "application/json", []byte("{}")); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after 5 failures begin = %d, want 429", resp.StatusCode)
	}
	resp, _ := do(t, c, "POST", ts.URL+"/login", "application/x-www-form-urlencoded",
		[]byte(url.Values{"password": {"secret"}}.Encode()))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("password after 5 passkey failures = %d, want 429 (shared budget)", resp.StatusCode)
	}
}

// Adding or removing a passkey needs a session opened in the last five
// minutes; an older one is sent to sign in again.
func TestPasskeyChangesNeedFreshSession(t *testing.T) {
	s, ts := passkeyServer(t)
	authr := virtualwebauthn.NewAuthenticator()
	owner := browser(t)
	passwordLogin(t, owner, ts)
	cred := register(t, owner, ts, &authr, "MacBook")

	// Age the session: the table stores only an expiry, a day less than the
	// full week means it was opened a day ago.
	u, _ := url.Parse(ts.URL)
	var tok string
	for _, ck := range owner.Jar.Cookies(u) {
		if ck.Name == "session" {
			tok = ck.Value
		}
	}
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token = ?`,
		time.Now().Add(6*24*time.Hour).Unix(), tok); err != nil {
		t.Fatal(err)
	}

	// Still signed in…
	if resp, _ := do(t, owner, "GET", ts.URL+"/passkeys", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("stale session /passkeys: %d, want 200", resp.StatusCode)
	}
	// …but not fresh enough to change anything.
	id := base64.RawURLEncoding.EncodeToString(cred.ID)
	for _, path := range []string{"/passkey/register/begin", "/passkey/register/finish", "/passkeys/" + id + "/delete"} {
		resp, _ := do(t, owner, "POST", ts.URL+path, "application/json", []byte("{}"))
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != reauthURL {
			t.Errorf("stale POST %s: %d %q; want 303 to %s", path, resp.StatusCode, resp.Header.Get("Location"), reauthURL)
		}
	}
	if _, page := do(t, owner, "GET", ts.URL+reauthURL, "", nil); !bytes.Contains(page, []byte("Confirm it")) {
		t.Error("reauth login page does not say so")
	}
	if n := countPasskeys(t, s); n != 1 {
		t.Fatalf("passkeys = %d after refused delete, want 1", n)
	}

	// Signing in again makes it fresh; now the delete goes through.
	passwordLogin(t, owner, ts)
	resp, _ := do(t, owner, "POST", ts.URL+"/passkeys/"+id+"/delete", "", nil)
	if resp.StatusCode != http.StatusSeeOther || countPasskeys(t, s) != 0 {
		t.Fatalf("fresh delete: %d, passkeys left %d", resp.StatusCode, countPasskeys(t, s))
	}
}

func TestPasskeysOffWithoutRPID(t *testing.T) {
	t.Setenv("WEBAUTHN_RP_ID", "")
	if passkeyConfig() != nil {
		t.Fatal("passkeys configured with no RP ID")
	}
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	cookies := loginSession(t, ts)
	for _, p := range []string{"/passkeys", "/static/passkey.js"} {
		if resp := get(t, ts, cookies, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with passkeys off: %d, want 404", p, resp.StatusCode)
		}
	}
	resp := postForm(t, ts, nil, "/passkey/login/begin", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /passkey/login/begin with passkeys off: %d, want 404", resp.StatusCode)
	}
	page := get(t, ts, cookies, "/")
	b, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if bytes.Contains(b, []byte("/passkeys")) {
		t.Error("console links to passkeys while they are off")
	}
}

func countPasskeys(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM passkeys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
