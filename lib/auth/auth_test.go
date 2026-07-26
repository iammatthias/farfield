package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVerifyPassword(t *testing.T) {
	tests := []struct {
		name            string
		input, expected string
		want            bool
	}{
		{"exact match", "correct-horse", "correct-horse", true},
		{"wrong password", "wrong", "correct-horse", false},
		{"prefix is not a match", "correct", "correct-horse", false},
		{"suffix is not a match", "horse", "correct-horse", false},
		{"case is significant", "Correct-Horse", "correct-horse", false},
		{"trailing space is significant", "correct-horse ", "correct-horse", false},
		// An unset PASSWORD must never authenticate. Callers check for the
		// empty expectation too, but the primitive has to hold on its own.
		{"empty input against empty expectation", "", "", true},
		{"any input against empty expectation", "anything", "", false},
		{"empty input against real password", "", "correct-horse", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifyPassword(tt.input, tt.expected); got != tt.want {
				t.Errorf("VerifyPassword(%q, %q) = %v, want %v",
					tt.input, tt.expected, got, tt.want)
			}
		})
	}
}

func TestVerifyAPIKey(t *testing.T) {
	if !VerifyAPIKey("ffk_abc123", "ffk_abc123") {
		t.Error("matching key rejected")
	}
	if VerifyAPIKey("ffk_abc124", "ffk_abc123") {
		t.Error("key differing in the last character accepted")
	}
	// Hashing both sides means length never leaks and a long guess is no
	// more expensive to reject than a short one.
	if VerifyAPIKey(strings.Repeat("x", 10000), "ffk_abc123") {
		t.Error("oversized key accepted")
	}
}

func TestNewSessionTokenIsRandomAndURLSafe(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		tok := NewSessionToken()
		if tok == "" {
			t.Fatal("empty session token")
		}
		if seen[tok] {
			t.Fatalf("session token %q issued twice in 1000 draws", tok)
		}
		seen[tok] = true
		// A token rides in a cookie, so it must need no escaping.
		for _, c := range tok {
			if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
				t.Fatalf("token %q contains %q, which is not cookie-safe", tok, c)
			}
		}
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	c := SessionCookie("tok123", true)
	if c.Value != "tok123" || c.Name != cookieName {
		t.Errorf("cookie = %s=%s, want %s=tok123", c.Name, c.Value, cookieName)
	}
	if !c.HttpOnly {
		t.Error("session cookie must be HttpOnly — script must not read it")
	}
	if !c.Secure {
		t.Error("secure=true must set the Secure attribute")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax (blocks cross-site form POSTs)", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}

	if insecure := SessionCookie("tok123", false); insecure.Secure {
		t.Error("secure=false must not set the Secure attribute (dev over HTTP)")
	}
}

func TestClearCookieExpiresImmediately(t *testing.T) {
	c := ClearCookie(true)
	if c.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative so the browser drops it now", c.MaxAge)
	}
	if c.Value != "" {
		t.Errorf("Value = %q, want empty", c.Value)
	}
	// The clearing cookie must match the one that was set, or the browser
	// keeps the original alongside it.
	set := SessionCookie("tok", true)
	if c.Name != set.Name || c.Path != set.Path || c.HttpOnly != set.HttpOnly {
		t.Error("clear cookie does not match the attributes of the set cookie")
	}
}

func TestSession(t *testing.T) {
	t.Run("reads the cookie", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: "tok123"})
		if tok, ok := Session(r); !ok || tok != "tok123" {
			t.Errorf("Session() = %q, %v; want tok123, true", tok, ok)
		}
	})
	t.Run("absent cookie", func(t *testing.T) {
		if _, ok := Session(httptest.NewRequest(http.MethodGet, "/", nil)); ok {
			t.Error("Session() reported ok with no cookie")
		}
	})
	t.Run("empty cookie is not a session", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: ""})
		if _, ok := Session(r); ok {
			t.Error("an empty cookie value was accepted as a session")
		}
	})
}

// Signed fleet sessions are what let one login span every app, so a forged or
// stale token must never verify.
func TestSignedSession(t *testing.T) {
	const secret = "fleet-secret"
	valid := SignSession(secret, "", time.Now().Add(time.Hour))

	if !VerifySignedSession(secret, "", valid) {
		t.Fatal("a freshly signed token did not verify")
	}

	t.Run("rejects", func(t *testing.T) {
		cases := map[string]string{
			"expired":            SignSession(secret, "", time.Now().Add(-time.Second)),
			"another secret":     SignSession("different-secret", "", time.Now().Add(time.Hour)),
			"tampered mac":       valid + "x",
			"truncated":          valid[:len(valid)-8],
			"empty":              "",
			"not a token":        "hello",
			"wrong prefix":       "v2" + strings.TrimPrefix(valid, "v1"),
			"too few segments":   "v1.999999999999.abc",
			"non-numeric expiry": "v1.notanumber.abc.def",
		}
		for name, tok := range cases {
			if VerifySignedSession(secret, "", tok) {
				t.Errorf("%s token verified", name)
			}
		}
	})

	// An empty secret means fleet sessions are off; nothing may verify then,
	// or an app with no SESSION_SECRET would accept forged tokens.
	t.Run("empty secret verifies nothing", func(t *testing.T) {
		if VerifySignedSession("", "", valid) {
			t.Error("a token verified against an empty secret")
		}
		if VerifySignedSession("", "", SignSession("", "", time.Now().Add(time.Hour))) {
			t.Error("a token signed with an empty secret verified")
		}
	})

	// Two tokens minted with the same secret and expiry must still differ —
	// the nonce is what keeps them from being a single reusable string.
	t.Run("tokens are unique", func(t *testing.T) {
		exp := time.Now().Add(time.Hour)
		if SignSession(secret, "", exp) == SignSession(secret, "", exp) {
			t.Error("two tokens with identical inputs came out identical")
		}
	})

	// The expiry is carried in the clear, so it must be covered by the MAC.
	t.Run("expiry is authenticated", func(t *testing.T) {
		parts := strings.SplitN(valid, ".", 4)
		if len(parts) != 4 {
			t.Fatalf("unexpected token shape: %q", valid)
		}
		far := strings.Join([]string{parts[0],
			"99999999999", parts[2], parts[3]}, ".")
		if VerifySignedSession(secret, "", far) {
			t.Error("a token whose expiry was extended in transit verified")
		}
	})
}

// The epoch is the fleet's revocation lever: bumping it must invalidate every
// token issued under the previous value, without any shared session store.
func TestSignedSessionEpochRevokes(t *testing.T) {
	const secret = "fleet-secret"
	exp := time.Now().Add(time.Hour)

	issued := SignSession(secret, "2026-01-01", exp)
	if !VerifySignedSession(secret, "2026-01-01", issued) {
		t.Fatal("a token did not verify under the epoch it was minted with")
	}
	if VerifySignedSession(secret, "2026-06-01", issued) {
		t.Error("a token survived an epoch change — revocation does not work")
	}
	// The unset epoch is its own value, not a wildcard.
	if VerifySignedSession(secret, "", issued) {
		t.Error("an empty epoch accepted a token minted under a real one")
	}
	if VerifySignedSession(secret, "2026-01-01", SignSession(secret, "", exp)) {
		t.Error("a real epoch accepted a token minted under the empty one")
	}
}

// The epoch is length-prefixed into the MAC, so no two (epoch, payload) pairs
// can collide by shifting the boundary between them.
func TestSignedSessionEpochIsUnambiguous(t *testing.T) {
	const secret = "fleet-secret"
	exp := time.Now().Add(time.Hour)
	tok := SignSession(secret, "ab", exp)
	for _, neighbour := range []string{"a", "b", "abc", "a.b", "ab."} {
		if VerifySignedSession(secret, neighbour, tok) {
			t.Errorf("epoch %q verified a token minted under %q", neighbour, "ab")
		}
	}
}
