package main

import (
	"log/slog"
	"net/http"
	"regexp"

	"github.com/iammatthias/farfield/lib/web"
)

// Who may see a paste. An id alone is not access: a private paste needs a
// session or a valid magic-link token, and the gate is the one place that
// decides — every view path asks it rather than re-deriving the rules.

// idPattern is the short content address shape: 16 lowercase base32 chars
// (every CID starts with the 'b' multibase prefix). Anything else — favicon
// probes, reserved words, truncations — is a clean 404 before any DB read.
var idPattern = regexp.MustCompile(`^[a-z2-7]{16}$`)

// reservedIDs are route words that must never resolve as paste ids. ServeMux
// literal precedence already routes them elsewhere; this is defense in depth.
var reservedIDs = map[string]bool{
	"login": true, "logout": true, "manage": true, "pastes": true,
	"status": true, "static": true, "api": true, "new": true,
}

func validID(id string) bool {
	return idPattern.MatchString(id) && !reservedIDs[id]
}

// Who may see a paste. An id alone is not access: a private paste needs a
// session or a valid magic-link token, and the gate is the one place that
// decides — every view path asks it rather than re-deriving the rules.

// sessionValid reports whether the request carries a live author session —
// fleet or database-backed, the same check RequireSession runs on the
// compose and manage pages, so the author's browser is recognized on paste
// views under either session scheme.
func (s *Server) sessionValid(r *http.Request) bool {
	return s.auth.SessionValid(r)
}

// presentedTokens collects every token credential on the request: the ?t=
// query, the X-Scrap-Token header, and any scrap_t cookies (set by a prior
// unlock; the browser may hold one per paste path).
func presentedTokens(r *http.Request) []string {
	var out []string
	if t := r.URL.Query().Get("t"); t != "" {
		out = append(out, t)
	}
	if t := r.Header.Get("X-Scrap-Token"); t != "" {
		out = append(out, t)
	}
	for _, c := range r.Cookies() {
		if c.Name == "scrap_t" && c.Value != "" {
			out = append(out, c.Value)
		}
	}
	return out
}

// gateResult is the outcome of running a paste's read gates.
type gateResult int

const (
	gateOK         gateResult = iota
	gateNotFound              // missing or invalid id
	gateGone                  // expired (row deleted as a side effect)
	gateNeedsLogin            // private, no session
	gateLocked                // token-gated, no credential presented
	gateForbidden             // token-gated, wrong credential
	gateLimited               // too many failed token attempts
	gateError                 // internal
)

// gate loads a paste and runs every read gate — existence, lazy expiry,
// private visibility, view token (with failure rate limiting). The author
// session bypasses the token gate. Both the HTML page and raw share it.
func (s *Server) gate(r *http.Request, id string) (*Paste, gateResult) {
	if !validID(id) {
		return nil, gateNotFound
	}
	p, err := getPaste(s.db, id)
	if err != nil {
		slog.Error("get paste", "err", err)
		return nil, gateError
	}
	if p == nil {
		return nil, gateNotFound
	}
	if expired(p) {
		if _, err := deletePaste(s.db, p.ID); err != nil {
			slog.Warn("could not delete expired paste", "id", p.ID, "err", err)
		}
		return nil, gateGone
	}
	authed := s.sessionValid(r)
	if p.Visibility == VisPrivate && !authed {
		return nil, gateNeedsLogin
	}
	if p.HasToken && !authed {
		tokens := presentedTokens(r)
		if len(tokens) == 0 {
			return nil, gateLocked
		}
		key := web.ClientIP(r) + "|" + p.ID
		if s.limiter.Blocked(key) {
			return nil, gateLimited
		}
		for _, t := range tokens {
			ok, err := verifyToken(s.db, p.ID, t)
			if err != nil {
				slog.Error("verify token", "err", err)
				return nil, gateError
			}
			if ok {
				return p, gateOK
			}
		}
		s.limiter.Fail(key)
		return nil, gateForbidden
	}
	return p, gateOK
}
