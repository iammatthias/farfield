package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// Signed fleet sessions: an HMAC-SHA256 token that any app holding the
// shared secret can verify offline — one login works across the whole fleet
// when the session cookie's Domain spans it. Stateless by design: there is
// no server-side session row, so "logout" clears the cookie rather than
// revoking the token; the embedded expiry bounds how long a leaked token
// lives. That trade fits a single-admin fleet.

const signedPrefix = "v1"

// SignSession mints a fleet session token that expires at the given time.
func SignSession(secret string, expires time.Time) string {
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	payload := strconv.FormatInt(expires.Unix(), 10) + "." +
		base64.RawURLEncoding.EncodeToString(nonce)
	return signedPrefix + "." + payload + "." + signSessionMAC(secret, payload)
}

// VerifySignedSession reports whether token is a well-formed, unexpired
// fleet session signed with secret.
func VerifySignedSession(secret, token string) bool {
	if secret == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != signedPrefix {
		return false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return false
	}
	payload := parts[1] + "." + parts[2]
	expect := signSessionMAC(secret, payload)
	return hmac.Equal([]byte(expect), []byte(parts[3]))
}

func signSessionMAC(secret, payload string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("ffsession." + signedPrefix + "." + payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
