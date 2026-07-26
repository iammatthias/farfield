package web

import (
	"database/sql"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
)

// Fleet-wide session config, read from the environment because it must be
// identical in every app for single sign-on to hold: SESSION_SECRET turns on
// signed fleet sessions (one login spans every app sharing the secret), and
// SESSION_COOKIE_DOMAIN widens the cookie to the fleet's parent domain
// (e.g. .farfield.systems). With no secret set, each app keeps its own
// database-backed sessions exactly as before.
var (
	fleetOnceAuth sync.Once
	fleetSecret   string
	cookieDomain  string
	fleetEpoch    string
)

func fleetSessionConfig() (secret, domain string) {
	loadFleetConfig()
	return fleetSecret, cookieDomain
}

// sessionEpoch is the fleet-wide revocation lever. Every signed session is
// bound to it, so changing SESSION_EPOCH and redeploying invalidates every
// outstanding token at once — the stateless equivalent of clearing a session
// table. Leave it unset until you need it; any value works, a timestamp reads
// best in a config.
func sessionEpoch() string {
	loadFleetConfig()
	return fleetEpoch
}

func loadFleetConfig() {
	fleetOnceAuth.Do(func() {
		fleetSecret = os.Getenv("SESSION_SECRET")
		cookieDomain = os.Getenv("SESSION_COOKIE_DOMAIN")
		fleetEpoch = os.Getenv("SESSION_EPOCH")
	})
}

// KeyChecker resolves an admin-issued token for an app to its scope —
// keys.ScopeRead / ScopeUpload / ScopeWrite. lib/keys implements it; the
// interface lives here so lib/web never depends on the key store.
type KeyChecker interface {
	Check(token, app string) (scope string, ok bool)
}

// Auth bundles the credentials and session storage an app's gated routes
// share. Zero-value fields fail closed: an empty Password rejects every
// login, an empty APIKey refuses every API write.
//
// ReadKey is the optional read-only bearer token. It is the one deliberately
// fail-open field: when empty, RequireReadKey leaves read endpoints public
// (their pre-token behavior), so a read token is opt-in per deployment.
//
// Keys + App optionally layer admin-issued keys (the keys app, lib/keys) on
// top of the env keys: a presented token that resolves for App is honored by
// the same gates, write scope where the APIKey is accepted and read-or-write
// scope where the ReadKey is. Env keys keep working unchanged. Assign Keys
// only from a successfully opened store — a typed-nil in the interface would
// read as configured.
type Auth struct {
	DB           *sql.DB
	Password     string
	APIKey       string
	ReadKey      string
	CookieSecure bool
	Keys         KeyChecker
	App          string

	// loginOnce/loginRL back the built-in login throttle. HandleLogin owns
	// it rather than leaving it to each app's route table, so no app can
	// ship an unthrottled front door by forgetting a wrapper.
	loginOnce sync.Once
	loginRL   *FailLimiter
}

// Login throttling: five wrong passwords per client per minute. Far above any
// human retry rate, far below a useful guessing rate. It matters more than it
// looks — with SESSION_SECRET set, a password cracked on any one app mints a
// session every app in the fleet accepts.
const (
	loginMaxFails   = 5
	loginFailWindow = time.Minute
)

func (a *Auth) loginLimiter() *FailLimiter {
	a.loginOnce.Do(func() {
		a.loginRL = NewFailLimiter(loginMaxFails, loginFailWindow)
	})
	return a.loginRL
}

// keyScope resolves the request's bearer token against the admin-issued key
// store, when one is attached.
func (a *Auth) keyScope(r *http.Request) (string, bool) {
	if a.Keys == nil {
		return "", false
	}
	return a.Keys.Check(APIKeyFrom(r), a.App)
}

// RequireSession guards the HTML admin UI. An invalid or absent session
// redirects to the login page. A signed fleet session (SESSION_SECRET) is
// accepted first; the app's own database sessions keep working either way,
// so enabling the secret never logs anyone out.
//
// State-changing requests must also pass the cross-origin check below, so a
// hostile page cannot drive an admin action on the strength of the session
// cookie alone.
func (a *Auth) RequireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowedOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if token, ok := auth.Session(r); ok {
			if secret, _ := fleetSessionConfig(); secret != "" &&
				auth.VerifySignedSession(secret, sessionEpoch(), token) {
				next(w, r)
				return
			}
			if a.DB != nil {
				if valid, err := store.ValidSession(a.DB, token); err == nil && valid {
					next(w, r)
					return
				}
			}
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// unsafeMethod reports whether a request can change state. GET/HEAD/OPTIONS
// are excluded: the admin UI never mutates on those.
func unsafeMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// allowedOrigin is the CSRF check for session-gated writes. The session
// cookie is SameSite=Lax, which already blocks a cross-site form POST in a
// current browser; this is the second layer, and it is the one that holds if
// that cookie attribute is ever relaxed or a browser is lenient about it.
//
// A request declaring an Origin (or, failing that, a Referer) must name this
// host or a sibling under SESSION_COOKIE_DOMAIN — the fleet's own apps. A
// request declaring neither is allowed: browsers always send Origin on
// cross-origin writes, so the header's absence means a non-browser client
// (curl, an iOS Shortcut), which carries no ambient cookie to abuse.
func allowedOrigin(r *http.Request) bool {
	if !unsafeMethod(r.Method) {
		return true
	}
	declared := r.Header.Get("Origin")
	if declared == "" || declared == "null" {
		declared = r.Header.Get("Referer")
	}
	if declared == "" {
		return true
	}
	u, err := url.Parse(declared)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	// Fleet siblings share the session cookie, so they are same-site by
	// construction; treat the configured cookie domain as the trust boundary.
	if _, domain := fleetSessionConfig(); domain != "" {
		suffix := "." + strings.TrimPrefix(domain, ".")
		host := u.Hostname()
		if strings.HasSuffix("."+host, suffix) {
			return true
		}
	}
	return false
}

// RequireAPIKey guards the JSON write endpoints. A missing or wrong key
// yields a 401. When neither an env key nor a key store is configured,
// writes are refused outright.
func (a *Auth) RequireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.APIKey == "" && a.Keys == nil {
			WriteError(w, http.StatusServiceUnavailable, "no API key configured")
			return
		}
		if a.HasWriteKey(r) {
			next(w, r)
			return
		}
		WriteError(w, http.StatusUnauthorized, "missing or invalid API key")
	}
}

// RequireReadKey guards read endpoints with the read-only bearer token,
// presented as "Authorization: Bearer <key>" or X-API-Key. The write APIKey is
// also accepted, since write access implies read access. When no ReadKey is
// configured the gate is open — set one (e.g. CONTENT_READ_KEY) to require a
// token for reads.
func (a *Auth) RequireReadKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.ReadKey == "" {
			next(w, r)
			return
		}
		if a.HasReadKey(r) {
			next(w, r)
			return
		}
		WriteError(w, http.StatusUnauthorized, "missing or invalid read token")
	}
}

// HasWriteKey reports whether the request carries a valid write credential —
// the env APIKey or an admin-issued write-scoped key. That is the privileged
// tier that unlocks writes and, for read endpoints, drafts. It is false when
// neither source is configured.
func (a *Auth) HasWriteKey(r *http.Request) bool {
	if a.APIKey != "" && auth.VerifyAPIKey(APIKeyFrom(r), a.APIKey) {
		return true
	}
	scope, ok := a.keyScope(r)
	return ok && scope == "write"
}

// HasReadKey reports whether the request presents a credential the read gate
// accepts: the read key, the write key (which implies read), or an
// admin-issued key with read or write scope. Unlike RequireReadKey, an
// unconfigured key is NOT treated as "open" here — with nothing to match, no
// request counts as privileged. It backs RequireReadKey and exempts trusted,
// keyed callers from rate limiting on otherwise-public endpoints.
func (a *Auth) HasReadKey(r *http.Request) bool {
	key := APIKeyFrom(r)
	if key == "" {
		return false
	}
	if (a.ReadKey != "" && auth.VerifyAPIKey(key, a.ReadKey)) ||
		(a.APIKey != "" && auth.VerifyAPIKey(key, a.APIKey)) {
		return true
	}
	scope, ok := a.keyScope(r)
	return ok && (scope == "read" || scope == "write")
}

// APIKeyFrom reads the API key from either an X-API-Key header or an
// "Authorization: Bearer <key>" header.
func APIKeyFrom(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	return ""
}

// HandleLogin verifies the posted password, opens a one-week session, and
// redirects to the admin index. Wire it to POST /login; the GET form stays
// app-owned (it renders through the app's templates).
//
// Failed attempts are rate-limited per client IP — see loginLimiter. A correct
// password is never throttled, so replaying a valid login stays free.
func (a *Auth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	ip := ClientIP(r)
	limiter := a.loginLimiter()
	if limiter.Blocked(ip) {
		http.Error(w, "too many attempts — try again shortly", http.StatusTooManyRequests)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if a.Password == "" || !auth.VerifyPassword(r.FormValue("password"), a.Password) {
		limiter.Fail(ip)
		http.Redirect(w, r, "/login?error=Invalid+password", http.StatusSeeOther)
		return
	}
	secret, domain := fleetSessionConfig()
	if secret != "" {
		// Fleet mode: a signed, stateless token every sibling app accepts.
		token := auth.SignSession(secret, sessionEpoch(), time.Now().Add(7*24*time.Hour))
		c := auth.SessionCookie(token, a.CookieSecure)
		c.Domain = domain
		http.SetCookie(w, c)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	token := auth.NewSessionToken()
	if err := store.InsertSession(a.DB, token, time.Now().Add(7*24*time.Hour)); err != nil {
		slog.Error("create session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, auth.SessionCookie(token, a.CookieSecure))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// HandleLogout deletes the session and clears the cookie. Fleet sessions
// are stateless, so their logout is the cookie clear itself — scoped to the
// same domain the login set, or the browser would keep the fleet cookie.
func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if token, ok := auth.Session(r); ok {
		_ = store.DeleteSession(a.DB, token)
	}
	c := auth.ClearCookie(a.CookieSecure)
	_, c.Domain = fleetSessionConfig()
	http.SetCookie(w, c)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
