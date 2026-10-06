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
//
// The token reads v1.<expires>.<nonce>~<issued>.<mac>. The issue time rides
// inside the nonce segment rather than in a new one: a verifier that predates
// it treats that segment as opaque bytes under the MAC, so a token minted by
// new code still opens an app still running old code (switchboard deploys on
// its own schedule). It is MAC-covered either way, so it cannot be forged
// fresh. Tokens minted before it existed simply carry no issue time, and
// read as not fresh — the safe direction.

const signedPrefix = "v1"

// issuedSep joins the nonce and the issue time. Outside the base64url
// alphabet, so it can never occur inside a nonce, and legal in a cookie.
const issuedSep = "~"

// SignSession mints a fleet session token, bound to epoch, issued now, that
// expires at the given time.
func SignSession(secret, epoch string, expires time.Time) string {
	return SignSessionAt(secret, epoch, time.Now(), expires)
}

// SignSessionAt is SignSession with an explicit issue time — for tests that
// need a session that is valid but no longer fresh.
func SignSessionAt(secret, epoch string, issued, expires time.Time) string {
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	payload := strconv.FormatInt(expires.Unix(), 10) + "." +
		base64.RawURLEncoding.EncodeToString(nonce) + issuedSep +
		strconv.FormatInt(issued.Unix(), 10)
	return signedPrefix + "." + payload + "." + signSessionMAC(secret, epoch, payload)
}

// VerifySignedSession reports whether token is a well-formed, unexpired fleet
// session signed with secret under the current epoch. A token minted under a
// different epoch fails the MAC check, which is what makes bumping the epoch
// a fleet-wide revocation.
func VerifySignedSession(secret, epoch, token string) bool {
	_, ok := verifySigned(secret, epoch, token)
	return ok
}

// SignedSessionIssued returns when a valid fleet session was minted. ok is
// false for an invalid token and for a valid one minted before tokens carried
// an issue time — "when did you last prove it was you" has no answer there,
// so callers asking for freshness must treat it as stale.
func SignedSessionIssued(secret, epoch, token string) (issued time.Time, ok bool) {
	nonce, valid := verifySigned(secret, epoch, token)
	if !valid {
		return time.Time{}, false
	}
	_, iat, found := strings.Cut(nonce, issuedSep)
	if !found {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(iat, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

// verifySigned checks token and, when it holds, returns its nonce segment.
func verifySigned(secret, epoch, token string) (string, bool) {
	if secret == "" {
		return "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != signedPrefix {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return "", false
	}
	payload := parts[1] + "." + parts[2]
	expect := signSessionMAC(secret, epoch, payload)
	if !hmac.Equal([]byte(expect), []byte(parts[3])) {
		return "", false
	}
	return parts[2], true
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
