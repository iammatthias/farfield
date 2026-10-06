package main

// Passkeys: the fleet's sign-in, run here because keys is the identity
// service. One user — the owner — under a fixed handle; any number of
// passkeys (one per device), each a row in keys.sqlite. A passkey login opens
// exactly the session a password login does (web.Auth.OpenSession), so with
// SESSION_SECRET and the fleet cookie domain it signs in every app at once.
//
// Off unless WEBAUTHN_RP_ID is set: then there are no passkey routes, no
// Passkeys link, and the login page is the password form it always was.

import (
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

const passkeySchema = `
CREATE TABLE IF NOT EXISTS passkeys (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	credential   TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	last_used_at TEXT
);`

// ownerHandle is the WebAuthn user handle. Fixed, because there is exactly
// one user and nothing to look up by it; it is not a secret.
const ownerHandle = "farfield-owner"

// freshWindow is how recently the session must have been opened to add or
// remove a passkey: a week-old cookie on an unlocked laptop must not be
// enough to plant a passkey that outlives any password change.
const freshWindow = 5 * time.Minute

// ceremonyCookie carries the id of an in-flight register/login ceremony.
// Scoped to /passkey/, the begin and finish routes, and nowhere else.
const (
	ceremonyCookie = "ff_ceremony"
	ceremonyPath   = "/passkey/"
	ceremonyTTL    = 5 * time.Minute
)

// passkeyConfig builds the relying party from the environment, or returns
// nil when passkeys are off. A misconfiguration logs and leaves them off —
// the password still works, so a typo in an origin never locks anyone out.
func passkeyConfig() *webauthn.WebAuthn {
	rpID := strings.TrimSpace(store.Env("WEBAUTHN_RP_ID", ""))
	if rpID == "" {
		return nil
	}
	var origins []string
	for _, o := range strings.Split(store.Env("WEBAUTHN_ORIGINS", ""), ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: store.Env("WEBAUTHN_RP_NAME", "farfield"),
		RPOrigins:     origins,
	})
	if err != nil {
		slog.Error("passkeys disabled: bad WebAuthn config", "rp_id", rpID, "err", err)
		return nil
	}
	return wa
}

// passkey is one stored credential, as the console lists it.
type passkey struct {
	ID       string
	Name     string
	Created  string
	LastUsed string
	cred     webauthn.Credential
}

func (s *Server) listPasskeys() ([]passkey, error) {
	rows, err := s.db.Query(`SELECT id, name, credential, created_at, last_used_at
		FROM passkeys ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []passkey
	for rows.Next() {
		var p passkey
		var raw string
		var used sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &raw, &p.Created, &used); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &p.cred); err != nil {
			return nil, err
		}
		p.LastUsed = used.String
		out = append(out, p)
	}
	return out, rows.Err()
}

func credID(c *webauthn.Credential) string {
	return base64.RawURLEncoding.EncodeToString(c.ID)
}

func (s *Server) insertPasskey(name string, c *webauthn.Credential) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO passkeys (id, name, credential, created_at)
		VALUES (?, ?, ?, ?)`, credID(c), name, string(raw), store.NowRFC3339())
	return err
}

// touchPasskey stores the credential as the login left it (sign count,
// flags) and stamps its use.
func (s *Server) touchPasskey(c *webauthn.Credential) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE passkeys SET credential = ?, last_used_at = ? WHERE id = ?`,
		string(raw), store.NowRFC3339(), credID(c))
	return err
}

// owner is the one WebAuthn user, carrying every stored passkey.
type owner struct{ creds []webauthn.Credential }

func (o owner) WebAuthnID() []byte                         { return []byte(ownerHandle) }
func (o owner) WebAuthnName() string                       { return "owner" }
func (o owner) WebAuthnDisplayName() string                { return "farfield" }
func (o owner) WebAuthnCredentials() []webauthn.Credential { return o.creds }

func (s *Server) owner() (owner, error) {
	ps, err := s.listPasskeys()
	if err != nil {
		return owner{}, err
	}
	o := owner{}
	for _, p := range ps {
		o.creds = append(o.creds, p.cred)
	}
	return o, nil
}

// ceremony is the server half of a register or login in progress.
type ceremony struct {
	kind string // "login" | "register"
	data webauthn.SessionData
	name string // register: the passkey's label
	next string // login: where to go after
}

func (s *Server) startCeremony(w http.ResponseWriter, c ceremony) {
	http.SetCookie(w, &http.Cookie{
		Name: ceremonyCookie, Value: s.ceremonies.put(c), Path: ceremonyPath,
		MaxAge: int(ceremonyTTL / time.Second), HttpOnly: true,
		Secure: s.auth.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

// endCeremony takes the request's ceremony (single use) and clears its cookie.
func (s *Server) endCeremony(w http.ResponseWriter, r *http.Request, kind string) (ceremony, bool) {
	http.SetCookie(w, &http.Cookie{
		Name: ceremonyCookie, Path: ceremonyPath, MaxAge: -1, HttpOnly: true,
		Secure: s.auth.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	ck, err := r.Cookie(ceremonyCookie)
	if err != nil {
		return ceremony{}, false
	}
	c, ok := s.ceremonies.take(ck.Value)
	return c, ok && c.kind == kind
}

// ── login ──────────────────────────────────────────────────────────────────

func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.beginRL.Allow(web.ClientIP(r)) || s.auth.LoginBlocked(r) {
		web.WriteError(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	var body struct {
		Next string `json:"next"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body)
	o, err := s.owner()
	if err != nil {
		s.fail(w, "load passkeys", err)
		return
	}
	if len(o.creds) == 0 {
		web.WriteError(w, http.StatusConflict, "no passkeys")
		return
	}
	opts, data, err := s.wa.BeginLogin(o, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		s.fail(w, "begin passkey login", err)
		return
	}
	s.startCeremony(w, ceremony{kind: "login", data: *data, next: web.SafeNext(r, body.Next)})
	w.Header().Set("Cache-Control", "no-store")
	web.WriteJSON(w, http.StatusOK, opts)
}

func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.auth.LoginBlocked(r) {
		web.WriteError(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	c, ok := s.endCeremony(w, r, "login")
	if !ok {
		s.auth.LoginFailed(r)
		web.WriteError(w, http.StatusBadRequest, "sign-in expired")
		return
	}
	o, err := s.owner()
	if err != nil {
		s.fail(w, "load passkeys", err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	cred, err := s.wa.FinishLogin(o, c.data, r)
	if err == nil && cred.Authenticator.CloneWarning {
		// A sign count that went backwards: two copies of one private key.
		err = errors.New("sign count regressed")
	}
	if err != nil {
		s.auth.LoginFailed(r)
		slog.Warn("passkey login refused", "err", err)
		web.WriteError(w, http.StatusUnauthorized, "passkey not accepted")
		return
	}
	if err := s.touchPasskey(cred); err != nil {
		slog.Warn("passkey: could not record use", "err", err)
	}
	if err := s.auth.OpenSession(w, r); err != nil {
		s.fail(w, "create session", err)
		return
	}
	next := c.next
	if next == "" {
		next = "/"
	}
	web.WriteJSON(w, http.StatusOK, map[string]string{"next": next})
}

// ── manage ─────────────────────────────────────────────────────────────────

// requireFresh is RequireSession plus freshness: a session older than
// freshWindow is sent to sign in again, then brought back to /passkeys.
func (s *Server) requireFresh(next http.HandlerFunc) http.HandlerFunc {
	return s.auth.RequireSession(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.SessionFresh(r, freshWindow) {
			http.Redirect(w, r, reauthURL, http.StatusSeeOther)
			return
		}
		next(w, r)
	})
}

const reauthURL = "/login?next=%2Fpasskeys&reauth=1"

func (s *Server) handlePasskeys(w http.ResponseWriter, r *http.Request) {
	ps, err := s.listPasskeys()
	if err != nil {
		s.fail(w, "list passkeys", err)
		return
	}
	s.rd.Render(w, "passkeys.html", map[string]any{
		"Passkeys": ps,
		"Fresh":    s.auth.SessionFresh(r, freshWindow),
		"Error":    r.URL.Query().Get("error"),
	})
}

func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "Passkey"
	}
	if utf8.RuneCountInString(name) > 64 {
		name = string([]rune(name)[:64])
	}
	o, err := s.owner()
	if err != nil {
		s.fail(w, "load passkeys", err)
		return
	}
	opts, data, err := s.wa.BeginRegistration(o,
		webauthn.WithExclusions(webauthn.Credentials(o.creds).CredentialDescriptors()),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		}))
	if err != nil {
		s.fail(w, "begin passkey registration", err)
		return
	}
	s.startCeremony(w, ceremony{kind: "register", data: *data, name: name})
	w.Header().Set("Cache-Control", "no-store")
	web.WriteJSON(w, http.StatusOK, opts)
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	c, ok := s.endCeremony(w, r, "register")
	if !ok {
		web.WriteError(w, http.StatusBadRequest, "expired — try again")
		return
	}
	o, err := s.owner()
	if err != nil {
		s.fail(w, "load passkeys", err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	cred, err := s.wa.FinishRegistration(o, c.data, r)
	if err != nil {
		slog.Warn("passkey registration refused", "err", err)
		web.WriteError(w, http.StatusBadRequest, "passkey not accepted")
		return
	}
	if err := s.insertPasskey(c.name, cred); err != nil {
		s.fail(w, "store passkey", err)
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]string{"id": credID(cred), "name": c.name})
}

func (s *Server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.Exec(`DELETE FROM passkeys WHERE id = ?`, r.PathValue("id")); err != nil {
		s.fail(w, "delete passkey", err)
		return
	}
	http.Redirect(w, r, "/passkeys", http.StatusSeeOther)
}

//go:embed static/passkey.js
var passkeyJS []byte

// handlePasskeyJS serves the ceremony script. The URL carries the theme
// version, but the script is this app's own, so it is revalidated rather
// than cached immutably.
func (s *Server) handlePasskeyJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(passkeyJS)
}
