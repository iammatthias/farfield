package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
)

// fleetMode puts HandleLogin on the stateless signed-session path, so these
// tests need no database to reach a successful login. Same idiom as
// TestFleetSession.
func fleetMode(t *testing.T) {
	t.Helper()
	fleetOnceAuth.Do(func() {})
	fleetSecret, cookieDomain = "login-test-secret", ""
	t.Cleanup(func() { fleetSecret, cookieDomain = "", "" })
}

// postLogin drives Auth.HandleLogin the way a browser would, from the given
// client address, and reports the response.
func postLogin(a *Auth, password, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	form := url.Values{"password": {password}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	a.HandleLogin(w, r)
	return w
}

// The whole point of moving the limiter into HandleLogin: an app that wires
// nothing but the handler still gets brute-force protection.
func TestHandleLoginThrottlesGuessing(t *testing.T) {
	fleetMode(t)
	a := &Auth{Password: "correct-horse"}

	for i := range loginMaxFails {
		w := postLogin(a, "wrong", "203.0.113.9:1234", nil)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("attempt %d: status = %d, want %d (redirect back to the form)",
				i+1, w.Code, http.StatusSeeOther)
		}
	}

	w := postLogin(a, "wrong", "203.0.113.9:1234", nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: status = %d, want %d",
			loginMaxFails+1, w.Code, http.StatusTooManyRequests)
	}

	// The budget is spent, so even the right password is refused until the
	// window rolls. That is the intended trade for a single-admin fleet.
	if w := postLogin(a, "correct-horse", "203.0.113.9:1234", nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked client: status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}
}

func TestHandleLoginBudgetIsPerClient(t *testing.T) {
	fleetMode(t)
	a := &Auth{Password: "correct-horse"}
	for range loginMaxFails + 3 {
		postLogin(a, "wrong", "203.0.113.9:1234", nil)
	}
	// A different client is untouched by the first one's failures.
	w := postLogin(a, "correct-horse", "198.51.100.4:5678", nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("second client: status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("second client redirected to %q, want %q (a successful login)", loc, "/")
	}
}

func TestHandleLoginSuccessIsNeverThrottled(t *testing.T) {
	fleetMode(t)
	a := &Auth{Password: "correct-horse"}
	// Replaying a valid login must stay free — only failures consume budget.
	for i := range loginMaxFails * 3 {
		w := postLogin(a, "correct-horse", "203.0.113.9:1234", nil)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("login %d: status = %d, want %d", i+1, w.Code, http.StatusSeeOther)
		}
	}
}

// A client that can reach the app directly must not be able to reset its own
// failure budget by rotating a forwarded-IP header.
func TestHandleLoginIgnoresSpoofedClientIP(t *testing.T) {
	fleetMode(t)
	a := &Auth{Password: "correct-horse"}
	const peer = "203.0.113.9:1234" // public: not a trusted proxy

	for i := range loginMaxFails {
		spoof := map[string]string{"CF-Connecting-IP": "10.0.0." + string(rune('1'+i))}
		if w := postLogin(a, "wrong", peer, spoof); w.Code != http.StatusSeeOther {
			t.Fatalf("attempt %d: status = %d, want %d", i+1, w.Code, http.StatusSeeOther)
		}
	}
	spoof := map[string]string{"CF-Connecting-IP": "10.0.0.99"}
	if w := postLogin(a, "wrong", peer, spoof); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a fresh spoofed header bought another attempt: status = %d, want %d",
			w.Code, http.StatusTooManyRequests)
	}
}

func TestClientIPTrustsForwardedHeadersOnlyFromPrivatePeers(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{{
		name:       "proxy on the container network is believed",
		remoteAddr: "172.18.0.5:40000",
		headers:    map[string]string{"CF-Connecting-IP": "198.51.100.7"},
		want:       "198.51.100.7",
	}, {
		name:       "loopback proxy is believed",
		remoteAddr: "127.0.0.1:40000",
		headers:    map[string]string{"X-Forwarded-For": "198.51.100.7, 10.0.0.1"},
		want:       "198.51.100.7",
	}, {
		name:       "direct public client cannot claim another address",
		remoteAddr: "203.0.113.9:40000",
		headers:    map[string]string{"CF-Connecting-IP": "198.51.100.7"},
		want:       "203.0.113.9",
	}, {
		name:       "no headers falls back to the socket peer",
		remoteAddr: "192.168.1.20:40000",
		want:       "192.168.1.20",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			if got := ClientIP(r); got != tt.want {
				t.Errorf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The session cookie is SameSite=Lax, which already blocks a cross-site form
// POST; this is the second layer, checked on every session-gated write.
func TestRequireSessionRefusesCrossOriginWrites(t *testing.T) {
	fleetMode(t)
	a := &Auth{}
	reached := false
	h := a.RequireSession(func(w http.ResponseWriter, r *http.Request) { reached = true })

	token := signedTestSession(t)
	tests := []struct {
		name     string
		method   string
		origin   string
		referer  string
		wantCode int
	}{
		{"same-origin POST", http.MethodPost, "https://content.farfield.systems", "", 0},
		{"no origin header (curl, a Shortcut)", http.MethodPost, "", "", 0},
		{"same-origin via Referer", http.MethodPost, "", "https://content.farfield.systems/entries", 0},
		{"hostile page POST", http.MethodPost, "https://evil.example", "", http.StatusForbidden},
		{"hostile page via Referer", http.MethodPost, "", "https://evil.example/x", http.StatusForbidden},
		{"opaque origin", http.MethodPost, "null", "https://evil.example/x", http.StatusForbidden},
		{"cross-origin DELETE", http.MethodDelete, "https://evil.example", "", http.StatusForbidden},
		// A lookalike domain must not pass the suffix check.
		{"suffix lookalike", http.MethodPost, "https://notfarfield.systems", "", http.StatusForbidden},
		// Reads are never blocked — the admin UI does not mutate on GET.
		{"cross-origin GET", http.MethodGet, "https://evil.example", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached = false
			r := httptest.NewRequest(tt.method, "https://content.farfield.systems/entries/x/delete", nil)
			r.Host = "content.farfield.systems"
			r.AddCookie(&http.Cookie{Name: "session", Value: token})
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.referer != "" {
				r.Header.Set("Referer", tt.referer)
			}
			w := httptest.NewRecorder()
			h(w, r)

			if tt.wantCode == http.StatusForbidden {
				if w.Code != http.StatusForbidden {
					t.Errorf("status = %d, want 403", w.Code)
				}
				if reached {
					t.Error("the handler ran for a cross-origin write")
				}
				return
			}
			if !reached {
				t.Errorf("legitimate request was refused (status %d)", w.Code)
			}
		})
	}
}

// A sibling app under SESSION_COOKIE_DOMAIN shares the session cookie, so it
// is same-site by construction and must not be refused.
func TestRequireSessionAllowsFleetSiblings(t *testing.T) {
	fleetOnceAuth.Do(func() {})
	fleetSecret, cookieDomain = "login-test-secret", ".farfield.systems"
	t.Cleanup(func() { fleetSecret, cookieDomain = "", "" })

	a := &Auth{}
	reached := false
	h := a.RequireSession(func(w http.ResponseWriter, r *http.Request) { reached = true })

	r := httptest.NewRequest(http.MethodPost, "https://blobs.farfield.systems/upload", nil)
	r.Host = "blobs.farfield.systems"
	r.Header.Set("Origin", "https://content.farfield.systems")
	r.AddCookie(&http.Cookie{Name: "session", Value: signedTestSession(t)})
	h(httptest.NewRecorder(), r)
	if !reached {
		t.Error("a fleet sibling's write was refused")
	}

	// A domain that merely ends in the same letters is not a sibling. This is
	// the bug a naive strings.HasSuffix on the bare domain would introduce.
	for _, hostile := range []string{
		"https://notfarfield.systems",
		"https://farfield.systems.evil.example",
		"https://evilfarfield.systems",
	} {
		reached = false
		r := httptest.NewRequest(http.MethodPost, "https://blobs.farfield.systems/upload", nil)
		r.Host = "blobs.farfield.systems"
		r.Header.Set("Origin", hostile)
		r.AddCookie(&http.Cookie{Name: "session", Value: signedTestSession(t)})
		w := httptest.NewRecorder()
		h(w, r)
		if reached || w.Code != http.StatusForbidden {
			t.Errorf("%s was treated as a fleet sibling (status %d)", hostile, w.Code)
		}
	}

	// The apex domain itself is a fleet member.
	reached = false
	apex := httptest.NewRequest(http.MethodPost, "https://blobs.farfield.systems/upload", nil)
	apex.Host = "blobs.farfield.systems"
	apex.Header.Set("Origin", "https://farfield.systems")
	apex.AddCookie(&http.Cookie{Name: "session", Value: signedTestSession(t)})
	h(httptest.NewRecorder(), apex)
	if !reached {
		t.Error("the apex domain was refused as a sibling")
	}
}

func signedTestSession(t *testing.T) string {
	t.Helper()
	return auth.SignSession(fleetSecret, sessionEpoch(), time.Now().Add(time.Hour))
}
