package web

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
)

// Fleet sign-in: one login page for every app. With FARFIELD_LOGIN_URL set
// (https://keys.farfield.systems/login), an unauthenticated browser on any
// console is sent there with ?next=<where it was going>, signs in once — by
// passkey or password — and lands back where it started, carrying a fleet
// cookie every sibling accepts. Each app's own /login keeps working: it is
// the fallback when the keys app is down, and the only way into an app the
// fleet cookie cannot reach (backup, on the tailnet).

// loginRedirect is where RequireSession sends a request with no session.
//
// The variable is read per request, not cached with the session config: it is
// a routing preference, not a security boundary, and tests flip it freely.
// Only a GET or HEAD carries next — that is a page the browser can simply
// load again; replaying a form POST as a GET after login would just 405.
func (a *Auth) loginRedirect(r *http.Request) string {
	login := os.Getenv("FARFIELD_LOGIN_URL")
	if login == "" {
		return "/login"
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return login
	}
	sep := "?"
	if strings.Contains(login, "?") {
		sep = "&"
	}
	return login + sep + url.Values{"next": {Origin(r) + r.URL.RequestURI()}}.Encode()
}

// ValidNext reports whether next is somewhere a login may send the browser
// afterwards. Without this check ?next= is an open redirect on the fleet's
// most trusted page — a phishing link that really does start at keys.
//
// Allowed: a same-origin path ("/passkeys", never "//host"); an absolute
// http(s) URL on this host; one on the fleet cookie domain (any sibling the
// session will open); and, only when this request itself arrived on loopback,
// a loopback URL — the dev fleet runs every app on its own localhost port. A
// production login never redirects to loopback.
func ValidNext(r *http.Request, next string) bool {
	if next == "" || len(next) > 2048 || strings.ContainsAny(next, "\\\r\n\t") {
		return false
	}
	if strings.HasPrefix(next, "/") {
		return !strings.HasPrefix(next, "//")
	}
	u, err := url.Parse(next)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	host := strings.ToLower(u.Hostname())
	if _, domain := fleetSessionConfig(); domain != "" {
		d := strings.ToLower(strings.TrimPrefix(domain, "."))
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return isLoopback(host) && isLoopback(requestHostname(r))
}

// SafeNext returns next when ValidNext allows it, else "".
func SafeNext(r *http.Request, next string) string {
	if ValidNext(r, next) {
		return next
	}
	return ""
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requestHostname is r.Host without its port or IPv6 brackets.
func requestHostname(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		return h
	}
	return strings.Trim(r.Host, "[]")
}

// SessionIssued reports when the request's session was opened. ok is false
// with no valid session, and for a fleet token minted before tokens carried
// an issue time. Database sessions store only their expiry; every session is
// granted the same sessionTTL, so the issue time is that expiry less the TTL.
func (a *Auth) SessionIssued(r *http.Request) (time.Time, bool) {
	token, ok := auth.Session(r)
	if !ok {
		return time.Time{}, false
	}
	if secret, _ := fleetSessionConfig(); secret != "" {
		if auth.VerifySignedSession(secret, sessionEpoch(), token) {
			return auth.SignedSessionIssued(secret, sessionEpoch(), token)
		}
	}
	if a.DB != nil {
		if exp, ok, err := store.SessionExpires(a.DB, token); err == nil && ok {
			return exp.Add(-sessionTTL), true
		}
	}
	return time.Time{}, false
}

// SessionFresh reports whether the request carries a session opened within
// the last d — "you proved it was you just now", which a week-old cookie on
// an unlocked laptop has not. Gate the actions that would hand an attacker
// the account for good (adding a passkey) on it, not merely on a session.
func (a *Auth) SessionFresh(r *http.Request, d time.Duration) bool {
	issued, ok := a.SessionIssued(r)
	return ok && time.Since(issued) <= d
}
