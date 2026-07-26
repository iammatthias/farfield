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
// when the session cookie's Domain spans it. Stateless by design: there is no
// server-side session row, so an ordinary logout clears the cookie rather
// than revoking the token, and the embedded expiry bounds how long a leaked
// one lives.
//
// The epoch is the revocation lever that statelessness would otherwise cost.
// It is mixed into the MAC, so changing it (SESSION_EPOCH in the fleet's env)
// invalidates every token ever issued under the previous value — the "log
// every session out everywhere, now" move, with no shared session store.

const signedPrefix = "v1"

// SignSession mints a fleet session token, bound to epoch, that expires at
// the given time.
func SignSession(secret, epoch string, expires time.Time) string {
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	payload := strconv.FormatInt(expires.Unix(), 10) + "." +
		base64.RawURLEncoding.EncodeToString(nonce)
	return signedPrefix + "." + payload + "." + signSessionMAC(secret, epoch, payload)
}

// VerifySignedSession reports whether token is a well-formed, unexpired fleet
// session signed with secret under the current epoch. A token minted under a
// different epoch fails the MAC check, which is what makes bumping the epoch
// a fleet-wide revocation.
func VerifySignedSession(secret, epoch, token string) bool {
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
	expect := signSessionMAC(secret, epoch, payload)
	return hmac.Equal([]byte(expect), []byte(parts[3]))
}

// signSessionMAC binds the epoch with a length prefix rather than plain
// concatenation, so no pair of (epoch, payload) values can produce the same
// MAC input as a different pair.
func signSessionMAC(secret, epoch, payload string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("ffsession." + signedPrefix + "."))
	m.Write([]byte(strconv.Itoa(len(epoch)) + ":" + epoch + "."))
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
