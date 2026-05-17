// Package auth provides password and API-key verification, token generation,
// and session cookie helpers for farfield apps. It is built entirely on the
// standard library and has no dependencies.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

// cookieName is the session cookie key used across farfield apps.
const cookieName = "session"

// constantTimeEqual reports whether a and b are equal without leaking their
// contents — or their lengths — through timing. Both sides are hashed first,
// so the compared buffers are always the same fixed size.
func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// VerifyPassword reports whether input matches the expected password, in
// constant time.
func VerifyPassword(input, expected string) bool {
	return constantTimeEqual(input, expected)
}

// VerifyAPIKey reports whether input matches the expected API key, in
// constant time.
func VerifyAPIKey(input, expected string) bool {
	return constantTimeEqual(input, expected)
}

// NewSessionToken returns a cryptographically random, URL-safe token suitable
// for use as a session identifier (crypto/rand.Text, 26 base32 characters).
func NewSessionToken() string {
	return rand.Text()
}

// NewAPIKey returns a cryptographically random, URL-safe API key.
func NewAPIKey() string {
	return rand.Text()
}

// SessionCookie builds a session cookie carrying token. Pass secure=true for
// HTTPS deployments so the cookie is never sent over plain HTTP.
func SessionCookie(token string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 7, // one week
	}
}

// ClearCookie returns a session cookie that expires immediately, ending the
// session on the client.
func ClearCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// Session reads the session token from the request's session cookie. The
// boolean is false when the cookie is absent or empty.
func Session(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}
