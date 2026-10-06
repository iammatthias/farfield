package main

// Native-app sign-in (RFC 8252): how the desktop client gets a key without
// anyone pasting one. The app opens the system browser at /device/authorize
// with a loopback redirect and a PKCE challenge; the owner, signed in here,
// taps Allow; the browser lands on the app's loopback listener with a
// one-time code; the app redeems code + verifier at /device/token for a
// freshly minted ffk_ key — revocable in this console like any other.
//
// The key is minted at redemption, not at Allow: a code nobody redeems
// leaves nothing behind.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iammatthias/farfield/lib/keys"
	"github.com/iammatthias/farfield/lib/web"
)

const (
	deviceClientID = "farfield-desktop"
	grantTTL       = 5 * time.Minute
	maxDeviceName  = 64
	maxState       = 512
)

// deviceRequest is a validated /device/authorize request.
type deviceRequest struct {
	RedirectURI string
	Challenge   string
	State       string
	Device      string
}

// grant is an approved authorization awaiting redemption, stored under its
// code.
type grant struct {
	challenge   string
	redirectURI string
	device      string
}

// parseDeviceRequest validates the authorize parameters, from the query (GET)
// or the confirm form (POST) alike — the POST re-validates everything, since
// the hidden fields come back from the browser.
func parseDeviceRequest(v url.Values) (deviceRequest, error) {
	if v.Get("client_id") != deviceClientID {
		return deviceRequest{}, errors.New("unknown client")
	}
	redirect := v.Get("redirect_uri")
	if _, ok := loopbackRedirect(redirect); !ok {
		return deviceRequest{}, errors.New("redirect_uri must be a loopback address")
	}
	if v.Get("code_challenge_method") != "S256" {
		return deviceRequest{}, errors.New("code_challenge_method must be S256")
	}
	challenge := v.Get("code_challenge")
	if !isB64URL(challenge, 43, 43) { // base64url of a SHA-256, unpadded
		return deviceRequest{}, errors.New("bad code_challenge")
	}
	state := v.Get("state")
	if len(state) > maxState {
		return deviceRequest{}, errors.New("state too long")
	}
	return deviceRequest{
		RedirectURI: redirect,
		Challenge:   challenge,
		State:       state,
		Device:      deviceName(v.Get("device")),
	}, nil
}

// loopbackRedirect accepts exactly http://127.0.0.1:<port><path>,
// http://[::1]:<port><path> or http://localhost:<port><path> (RFC 8252 §7.3).
// Anything else could carry the code off the machine, so it is refused
// outright — the caller shows a 400 and never redirects to it.
func loopbackRedirect(raw string) (*url.URL, bool) {
	if raw == "" || strings.ContainsAny(raw, "#\\") {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" {
		return nil, false
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, false
	}
	switch strings.TrimSuffix(u.Host, ":"+u.Port()) {
	case "127.0.0.1", "[::1]", "localhost":
	default:
		return nil, false
	}
	if u.Path != "" && !strings.HasPrefix(u.Path, "/") {
		return nil, false
	}
	return u, true
}

func isB64URL(s string, minLen, maxLen int) bool {
	if len(s) < minLen || len(s) > maxLen {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// deviceName cleans the label the client sent: printable, trimmed, bounded.
// It ends up in a key name and on the confirm page, nowhere it could execute.
func deviceName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxDeviceName {
		s = strings.TrimSpace(string([]rune(s)[:maxDeviceName]))
	}
	if s == "" {
		s = "desktop"
	}
	return s
}

// withQuery returns the redirect URI with params merged into its query, so a
// client that put its own query on the loopback URL keeps it.
func withQuery(redirectURI string, params url.Values) string {
	u, _ := loopbackRedirect(redirectURI)
	q := u.Query()
	for k, vs := range params {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) deviceError(w http.ResponseWriter, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	s.rd.Render(w, "device.html", map[string]any{"Error": msg})
}

// handleDeviceAuthorize shows the confirm page. Parameters are checked before
// the session, so a bad request is a 400 for everyone and the login page is
// never asked to carry a request that could not succeed.
func (s *Server) handleDeviceAuthorize(w http.ResponseWriter, r *http.Request) {
	req, err := parseDeviceRequest(r.URL.Query())
	if err != nil {
		s.deviceError(w, err.Error())
		return
	}
	if !s.auth.SessionValid(r) {
		http.Redirect(w, r, "/login?"+url.Values{"next": {r.URL.RequestURI()}}.Encode(),
			http.StatusSeeOther)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.rd.Render(w, "device.html", map[string]any{"Req": req, "Client": deviceClientID})
}

// handleDeviceDecide is the confirm form's POST, behind RequireSession (and
// so its cross-origin check).
func (s *Server) handleDeviceDecide(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.deviceError(w, "bad form")
		return
	}
	req, err := parseDeviceRequest(r.PostForm)
	if err != nil {
		s.deviceError(w, err.Error())
		return
	}
	q := url.Values{}
	if req.State != "" {
		q.Set("state", req.State)
	}
	if r.PostFormValue("action") != "allow" {
		q.Set("error", "access_denied")
		http.Redirect(w, r, withQuery(req.RedirectURI, q), http.StatusSeeOther)
		return
	}
	code := s.grants.put(grant{
		challenge:   req.Challenge,
		redirectURI: req.RedirectURI,
		device:      req.Device,
	})
	q.Set("code", code)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, withQuery(req.RedirectURI, q), http.StatusSeeOther)
}

// tokenRequest is the /device/token body, form-encoded or JSON.
type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
}

// handleDeviceToken redeems a code. It sits behind web.PrivateIngress — the
// tunnel never reaches it — and asks for no API key: the code plus its PKCE
// verifier is the credential. Every failure is the same 400 invalid_grant,
// the grant is gone after any attempt, and failures are throttled per client.
// Nothing here is logged: not the code, the verifier, or the token.
func (s *Server) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	if s.deviceFails.Blocked(web.ClientIP(r)) {
		web.WriteError(w, http.StatusTooManyRequests, "slow_down")
		return
	}
	var tr tokenRequest
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if json.NewDecoder(r.Body).Decode(&tr) != nil {
			tr = tokenRequest{}
		}
	} else if r.ParseForm() == nil {
		tr = tokenRequest{
			GrantType:    r.PostForm.Get("grant_type"),
			Code:         r.PostForm.Get("code"),
			CodeVerifier: r.PostForm.Get("code_verifier"),
			RedirectURI:  r.PostForm.Get("redirect_uri"),
		}
	}
	// Take first: from here the code is spent, whatever happens next.
	g, ok := s.grants.take(tr.Code)
	if !ok || tr.GrantType != "authorization_code" ||
		tr.RedirectURI != g.redirectURI || !verifierMatches(tr.CodeVerifier, g.challenge) {
		s.deviceFails.Fail(web.ClientIP(r))
		web.WriteError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	token, k, err := s.ks.Mint("Farfield on "+g.device, keys.AppAny, keys.ScopeWrite, time.Time{})
	if err != nil {
		s.fail(w, "mint device key", err)
		return
	}
	w.Header().Set("Pragma", "no-cache")
	web.WriteJSON(w, http.StatusOK, map[string]string{
		"access_token": token,
		"token_type":   "api-key",
		"key_id":       k.ID,
		"app":          k.App,
		"scope":        k.Scope,
	})
}

// verifierMatches checks PKCE S256: base64url(sha256(verifier)) == challenge.
// RFC 7636 verifiers are 43–128 unreserved characters.
func verifierMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, c := range verifier {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}
