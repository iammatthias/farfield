package main

import (
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

// maxPasteBytes bounds a paste body — text only, and 2 MiB of text is a very
// long paste.
const maxPasteBytes = 2 << 20

// composeLangs is the curated lang select on the compose form. The API
// accepts any chroma lexer name; unknown values render plain.
var composeLangs = []string{
	"bash", "c", "cpp", "css", "diff", "dockerfile", "go", "html", "java",
	"javascript", "json", "kotlin", "markdown", "python", "ruby", "rust",
	"sql", "swift", "toml", "typescript", "yaml",
}

// Server holds the running scrap service.
type Server struct {
	db        *sql.DB
	auth      *web.Auth
	rd        *web.Renderer
	publicURL string        // absolute base for URLs the API returns
	limiter   *tokenLimiter // failed token attempts, per IP+paste
	chromaCSS template.CSS  // highlight stylesheet, embedded into view pages

	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder
}

// run wires up dependencies and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("SCRAP_DB_PATH", "scrap.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	s := newServer(db,
		store.Env("PASSWORD", ""),
		store.Env("SCRAP_API_KEY", ""),
		store.Env("COOKIE_SECURE", "false") == "true",
		store.Env("SCRAP_PUBLIC_URL", "http://"+net.JoinHostPort("127.0.0.1", port)))
	if err := s.parseTemplates(); err != nil {
		return err
	}

	// Expiry is enforced lazily on read; the sweep keeps the table itself
	// from accumulating dead rows nobody reads.
	go s.sweepLoop()

	s.pulse = pulse.New(s.db, "scrap")
	defer s.pulse.Close()
	return web.Serve(host, port, s.routes())
}

// newServer builds a Server without templates (parseTemplates) or routes —
// split out so tests can assemble one against a temp database.
func newServer(db *sql.DB, password, apiKey string, cookieSecure bool, publicURL string) *Server {
	return &Server{
		db: db,
		auth: &web.Auth{
			DB:           db,
			Password:     password,
			APIKey:       apiKey,
			CookieSecure: cookieSecure,
		},
		publicURL: strings.TrimRight(publicURL, "/"),
		limiter:   newTokenLimiter(5, time.Minute),
		chromaCSS: highlightCSS(),
	}
}

func (s *Server) parseTemplates() error {
	tmpl, err := web.ParseTemplates(assets, template.FuncMap{
		"relAge":   relAge,
		"ttl":      ttlText,
		"sizeText": sizeText,
	})
	if err != nil {
		return err
	}
	s.rd = &web.Renderer{Templates: tmpl, AssetVer: theme.Version}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Author UI — session-gated. Compose lives at /; manage is the table.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleCompose))
	mux.HandleFunc("POST /pastes", s.auth.RequireSession(s.handleCreate))
	mux.HandleFunc("GET /manage", s.auth.RequireSession(s.handleManage))
	mux.HandleFunc("POST /pastes/{id}/delete", s.auth.RequireSession(s.handleDelete))
	mux.HandleFunc("POST /pastes/expired/delete", s.auth.RequireSession(s.handleDeleteExpired))

	// Token lifecycle — roll replaces, set attaches, remove deletes. Roll and
	// set surface the fresh secret on the shown-once confirmation page.
	mux.HandleFunc("POST /pastes/{id}/token/roll", s.auth.RequireSession(s.handleTokenRoll))
	mux.HandleFunc("POST /pastes/{id}/token/set", s.auth.RequireSession(s.handleTokenSet))
	mux.HandleFunc("POST /pastes/{id}/token/remove", s.auth.RequireSession(s.handleTokenRemove))

	// Login.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// Public reads. Literal /pastes outranks the /{id} wildcard in ServeMux
	// precedence, as do /login, /status, and /static/*.
	mux.HandleFunc("GET /pastes", s.handlePublicIndex)
	mux.HandleFunc("GET /{id}", s.handleView)
	mux.HandleFunc("GET /{id}/raw", s.handleRaw)
	mux.HandleFunc("POST /{id}/unlock", s.handleUnlock)

	// Terminal API — raw text in, a URL out.
	mux.HandleFunc("POST /api/pastes", s.auth.RequireAPIKey(s.handleAPICreate))
	mux.HandleFunc("DELETE /api/pastes/{id}", s.auth.RequireAPIKey(s.handleAPIDelete))
	mux.HandleFunc("POST /api/pastes/{id}/token/roll", s.auth.RequireAPIKey(s.handleAPITokenRoll))
	mux.HandleFunc("DELETE /api/pastes/{id}/token", s.auth.RequireAPIKey(s.handleAPITokenRemove))

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// Everything scrap serves is text, so Gzip wraps the whole mux. Logging
	// sits outside so the recorded status is the final one; pulse traffic
	// recording sits innermost so logged timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(s.pulse.Wrap(mux))),
		"GET", "POST", "DELETE", "OPTIONS")
}

// sweepLoop deletes expired pastes hourly (and once at startup).
func (s *Server) sweepLoop() {
	for {
		if n, err := deleteExpiredPastes(s.db); err != nil {
			slog.Warn("expiry sweep failed", "err", err)
		} else if n > 0 {
			slog.Info("expiry sweep", "deleted", n)
		}
		time.Sleep(time.Hour)
	}
}

// ── id validation ──────────────────────────────────────────────────────────

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

// ── view gates ─────────────────────────────────────────────────────────────

// sessionValid reports whether the request carries a live author session.
func (s *Server) sessionValid(r *http.Request) bool {
	token, ok := auth.Session(r)
	if !ok {
		return false
	}
	valid, err := store.ValidSession(s.db, token)
	return err == nil && valid
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
		key := clientIP(r) + "|" + p.ID
		if s.limiter.blocked(key) {
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
		s.limiter.fail(key)
		return nil, gateForbidden
	}
	return p, gateOK
}

// ── public view handlers ───────────────────────────────────────────────────

func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, res := s.gate(r, id)
	switch res {
	case gateOK:
	case gateNotFound:
		http.NotFound(w, r)
		return
	case gateGone:
		w.WriteHeader(http.StatusGone)
		s.rd.Render(w, "gone.html", nil)
		return
	case gateNeedsLogin:
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case gateLocked:
		s.renderLocked(w, id, http.StatusUnauthorized, "")
		return
	case gateForbidden:
		s.renderLocked(w, id, http.StatusForbidden, "Wrong token.")
		return
	case gateLimited:
		s.renderLocked(w, id, http.StatusTooManyRequests,
			"Too many attempts. Wait a minute.")
		return
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	code, err := highlightHTML(p.Body, p.Lang)
	if err != nil {
		slog.Error("highlight", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := incrementViews(s.db, p.ID); err != nil {
		slog.Warn("could not count view", "err", err)
	} else {
		p.Views++
	}
	lang := p.Lang
	if lang == "" || !knownLang(lang) {
		lang = "plain"
	}
	s.rd.Render(w, "view.html", map[string]any{
		"Paste":     p,
		"Lang":      lang,
		"Code":      code,
		"ChromaCSS": s.chromaCSS,
		"Authed":    s.sessionValid(r),
	})
}

func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	p, res := s.gate(r, r.PathValue("id"))
	switch res {
	case gateOK:
	case gateNotFound:
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return
	case gateGone:
		web.WriteError(w, http.StatusGone, "paste expired")
		return
	case gateNeedsLogin:
		web.WriteError(w, http.StatusUnauthorized, "session required")
		return
	case gateLocked:
		web.WriteError(w, http.StatusUnauthorized, "token required")
		return
	case gateForbidden:
		web.WriteError(w, http.StatusForbidden, "wrong token")
		return
	case gateLimited:
		web.WriteError(w, http.StatusTooManyRequests, "too many token attempts")
		return
	default:
		web.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := incrementViews(s.db, p.ID); err != nil {
		slog.Warn("could not count view", "err", err)
	}
	w.Header().Set("ETag", `"`+p.CID+`"`)
	if web.ETagMatch(r, p.CID) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, p.Body)
}

// handleUnlock accepts a typed (or magic-link-posted) token, sets a per-paste
// cookie on success, and bounces back to the view.
func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	p, err := getPaste(s.db, id)
	if err != nil {
		slog.Error("get paste", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.NotFound(w, r)
		return
	}
	if expired(p) {
		if _, err := deletePaste(s.db, p.ID); err != nil {
			slog.Warn("could not delete expired paste", "id", p.ID, "err", err)
		}
		w.WriteHeader(http.StatusGone)
		s.rd.Render(w, "gone.html", nil)
		return
	}
	if p.Visibility == VisPrivate && !s.sessionValid(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !p.HasToken {
		http.Redirect(w, r, "/"+id, http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	token := r.FormValue("token")
	key := clientIP(r) + "|" + id
	if s.limiter.blocked(key) {
		s.renderLocked(w, id, http.StatusTooManyRequests,
			"Too many attempts. Wait a minute.")
		return
	}
	ok, err := verifyToken(s.db, id, token)
	if err != nil {
		slog.Error("verify token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		s.limiter.fail(key)
		s.renderLocked(w, id, http.StatusForbidden, "Wrong token.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "scrap_t",
		Value:    token,
		Path:     "/" + id,
		HttpOnly: true,
		Secure:   s.auth.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 7,
	})
	http.Redirect(w, r, "/"+id, http.StatusSeeOther)
}

// renderLocked serves the unlock shell. It deliberately receives only the id
// — no paste fields — so a locked page cannot leak title, lang, or body.
func (s *Server) renderLocked(w http.ResponseWriter, id string, status int, errMsg string) {
	w.WriteHeader(status)
	s.rd.Render(w, "locked.html", map[string]any{
		"ID":    id,
		"Error": errMsg,
	})
}

// handlePublicIndex lists public, unexpired pastes — title/lang/age only.
func (s *Server) handlePublicIndex(w http.ResponseWriter, r *http.Request) {
	ps, err := listPublicPastes(s.db)
	if err != nil {
		slog.Error("list public pastes", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.rd.Render(w, "index.html", map[string]any{
		"Pastes": ps,
		"Authed": s.sessionValid(r),
	})
}

// ── author UI ──────────────────────────────────────────────────────────────

func (s *Server) handleCompose(w http.ResponseWriter, r *http.Request) {
	s.renderCompose(w, http.StatusOK, map[string]any{"Visibility": VisUnlisted})
}

func (s *Server) renderCompose(w http.ResponseWriter, status int, form map[string]any) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	form["Langs"] = composeLangs
	form["Expiries"] = expiryChoices
	s.rd.Render(w, "compose.html", form)
}

// handleCreate is the browser compose POST.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPasteBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	body := r.FormValue("body")
	form := map[string]any{
		"Body":       body,
		"Title":      strings.TrimSpace(r.FormValue("title")),
		"Lang":       strings.ToLower(strings.TrimSpace(r.FormValue("lang"))),
		"Visibility": r.FormValue("visibility"),
		"Expires":    r.FormValue("expires"),
	}
	if strings.TrimSpace(body) == "" {
		form["Error"] = "Body is required — paste something."
		s.renderCompose(w, http.StatusBadRequest, form)
		return
	}
	tokenSecret := strings.TrimSpace(r.FormValue("token"))
	if r.FormValue("token_generate") == "on" {
		tokenSecret = auth.NewSessionToken()
	}
	p, err := s.createPaste(body, form["Title"].(string), form["Lang"].(string),
		r.FormValue("visibility"), r.FormValue("expires"), tokenSecret)
	if err != nil {
		form["Error"] = err.Error()
		s.renderCompose(w, http.StatusBadRequest, form)
		return
	}
	if tokenSecret != "" {
		// The token is hashed at rest and unrecoverable, and the author's
		// session bypasses the gate on /{id} — a redirect would render the
		// paste normally and the secret would be gone. Surface it exactly
		// once, on a server-rendered confirmation, before sending them on.
		s.rd.Render(w, "created.html", map[string]any{
			"ID":        p.ID,
			"Title":     p.Title,
			"PasteURL":  s.publicURL + "/" + p.ID,
			"MagicLink": s.magicLink(p.ID, tokenSecret),
			"Token":     tokenSecret,
		})
		return
	}
	http.Redirect(w, r, "/"+p.ID, http.StatusSeeOther)
}

// magicLink builds the shareable unlock URL. The secret rides the fragment so
// it never reaches server logs; PathEscape (never QueryEscape, whose "+" for
// space survives decodeURIComponent as a literal plus) keeps the unlock
// shell's decode exact for typed passphrases.
func (s *Server) magicLink(id, secret string) string {
	return s.publicURL + "/" + id + "#t=" + url.PathEscape(secret)
}

// createPaste normalizes, upserts, and applies token + visibility rules —
// the shared core of the browser and API create paths.
func (s *Server) createPaste(body, title, lang, visibility, expires, tokenSecret string) (*Paste, error) {
	if !validVisibility(visibility) {
		visibility = VisUnlisted
	}
	expiresAt, err := parseExpiry(strings.TrimSpace(expires))
	if err != nil {
		return nil, fmt.Errorf("expiry must be one of %s", strings.Join(expiryChoices, ", "))
	}
	short, full := pasteID(body)

	// A token (newly set here, or already on the existing row for this same
	// content) forces visibility down to at least unlisted — a locked paste
	// must never advertise itself on the public index.
	hasToken := tokenSecret != ""
	if !hasToken {
		if existing, err := getPaste(s.db, short); err != nil {
			return nil, err
		} else if existing != nil && existing.HasToken {
			hasToken = true
		}
	}
	if hasToken && visibility == VisPublic {
		visibility = VisUnlisted
	}

	p := &Paste{
		ID:         short,
		CID:        full,
		Title:      title,
		Lang:       lang,
		Body:       body,
		Visibility: visibility,
		ExpiresAt:  expiresAt,
	}
	if err := upsertPaste(s.db, p); err != nil {
		return nil, err
	}
	if tokenSecret != "" {
		if err := setToken(s.db, p.ID, tokenSecret, ""); err != nil {
			return nil, err
		}
	}
	p.HasToken = hasToken
	return p, nil
}

func (s *Server) handleManage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	visibility := q.Get("visibility")
	if !validVisibility(visibility) {
		visibility = ""
	}
	lang := strings.ToLower(strings.TrimSpace(q.Get("lang")))
	search := strings.TrimSpace(q.Get("q"))

	ps, err := listManagePastes(s.db, visibility, lang, search)
	if err != nil {
		slog.Error("list pastes", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	langs, err := distinctLangs(s.db)
	if err != nil {
		slog.Error("list langs", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	total, err := countPastes(s.db)
	if err != nil {
		slog.Error("count pastes", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.rd.Render(w, "manage.html", map[string]any{
		"Pastes":     ps,
		"Langs":      langs,
		"Total":      total,
		"Shown":      len(ps),
		"Visibility": visibility,
		"Lang":       lang,
		"Q":          search,
	})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := deletePaste(s.db, r.PathValue("id")); err != nil {
		slog.Error("delete paste", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/manage", http.StatusSeeOther)
}

func (s *Server) handleDeleteExpired(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteExpiredPastes(s.db); err != nil {
		slog.Error("delete expired", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/manage", http.StatusSeeOther)
}

// ── token lifecycle ────────────────────────────────────────────────────────

// livePaste loads an unexpired paste for a token-lifecycle handler, or
// returns nil after writing the 404/410. Errors are JSON (web.WriteError) on
// both surfaces — these are POST/DELETE endpoints, not pages.
func (s *Server) livePaste(w http.ResponseWriter, id string) *Paste {
	if !validID(id) {
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return nil
	}
	p, err := getPaste(s.db, id)
	if err != nil {
		slog.Error("get paste", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "internal error")
		return nil
	}
	if p == nil {
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return nil
	}
	if expired(p) {
		if _, err := deletePaste(s.db, p.ID); err != nil {
			slog.Warn("could not delete expired paste", "id", p.ID, "err", err)
		}
		web.WriteError(w, http.StatusGone, "paste expired")
		return nil
	}
	return p
}

// freshToken generates a new 26-char secret and installs it as the paste's
// one token (cap-1 set semantics — any previous secret stops working
// immediately). A public paste is forced down to unlisted, same as create.
func (s *Server) freshToken(p *Paste) (string, error) {
	if p.Visibility == VisPublic {
		if err := setVisibility(s.db, p.ID, VisUnlisted); err != nil {
			return "", err
		}
		p.Visibility = VisUnlisted
	}
	secret := auth.NewSessionToken()
	if err := setToken(s.db, p.ID, secret, ""); err != nil {
		return "", err
	}
	p.HasToken = true
	return secret, nil
}

// renderTokenConfirmation reuses the shown-once create confirmation for a
// rolled/set token — the secret is hashed at rest, so this page is the only
// time it is ever visible.
func (s *Server) renderTokenConfirmation(w http.ResponseWriter, p *Paste, label, secret string) {
	s.rd.Render(w, "created.html", map[string]any{
		"Label":     label,
		"ID":        p.ID,
		"Title":     p.Title,
		"PasteURL":  s.publicURL + "/" + p.ID,
		"MagicLink": s.magicLink(p.ID, secret),
		"Token":     secret,
	})
}

// handleTokenRoll replaces an existing token with a fresh secret.
func (s *Server) handleTokenRoll(w http.ResponseWriter, r *http.Request) {
	p := s.livePaste(w, r.PathValue("id"))
	if p == nil {
		return
	}
	if !p.HasToken {
		web.WriteError(w, http.StatusConflict, "no token set — add one instead")
		return
	}
	secret, err := s.freshToken(p)
	if err != nil {
		slog.Error("roll token", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not roll token")
		return
	}
	s.renderTokenConfirmation(w, p, "Token rolled", secret)
}

// handleTokenSet attaches a token to a paste that has none.
func (s *Server) handleTokenSet(w http.ResponseWriter, r *http.Request) {
	p := s.livePaste(w, r.PathValue("id"))
	if p == nil {
		return
	}
	if p.HasToken {
		web.WriteError(w, http.StatusConflict, "token already set — roll it instead")
		return
	}
	secret, err := s.freshToken(p)
	if err != nil {
		slog.Error("set token", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not set token")
		return
	}
	s.renderTokenConfirmation(w, p, "Token set", secret)
}

// handleTokenRemove deletes the token row(s); the paste serves per its
// visibility again.
func (s *Server) handleTokenRemove(w http.ResponseWriter, r *http.Request) {
	p := s.livePaste(w, r.PathValue("id"))
	if p == nil {
		return
	}
	if _, err := deleteTokens(s.db, p.ID); err != nil {
		slog.Error("remove token", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not remove token")
		return
	}
	http.Redirect(w, r, "/manage", http.StatusSeeOther)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

// ── terminal API ───────────────────────────────────────────────────────────

// handleAPICreate accepts a raw text body (any Content-Type) and returns the
// paste URL as text/plain — pipe-clean:
//
//	cat x.go | curl --data-binary @- -H "X-API-Key: $K" \
//	    "https://scrap.../api/pastes?lang=go&expires=1d&token=generate"
func (s *Server) handleAPICreate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPasteBytes))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(string(body)) == "" {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	tokenParam := q.Get("token")
	tokenSecret := tokenParam
	generated := tokenParam == "generate"
	if generated {
		tokenSecret = auth.NewSessionToken()
	}
	p, err := s.createPaste(string(body),
		strings.TrimSpace(q.Get("title")),
		strings.ToLower(strings.TrimSpace(q.Get("lang"))),
		q.Get("visibility"), q.Get("expires"), tokenSecret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintln(w, s.publicURL+"/"+p.ID)
	if generated {
		fmt.Fprintln(w, "token: "+tokenSecret)
	}
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return
	}
	existed, err := deletePaste(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete paste")
		return
	}
	if !existed {
		web.WriteError(w, http.StatusNotFound, "paste not found")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleAPITokenRoll is the terminal twin of the manage-view roll: replace
// the existing token and print the fresh secret, pipe-clean.
func (s *Server) handleAPITokenRoll(w http.ResponseWriter, r *http.Request) {
	p := s.livePaste(w, r.PathValue("id"))
	if p == nil {
		return
	}
	if !p.HasToken {
		web.WriteError(w, http.StatusConflict, "no token set")
		return
	}
	secret, err := s.freshToken(p)
	if err != nil {
		slog.Error("roll token", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not roll token")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "token: "+secret)
}

// handleAPITokenRemove deletes a paste's token(s) over the terminal API.
func (s *Server) handleAPITokenRemove(w http.ResponseWriter, r *http.Request) {
	p := s.livePaste(w, r.PathValue("id"))
	if p == nil {
		return
	}
	existed, err := deleteTokens(s.db, p.ID)
	if err != nil {
		slog.Error("remove token", "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not remove token")
		return
	}
	if !existed {
		web.WriteError(w, http.StatusNotFound, "no token set")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"tokenRemoved": p.ID})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countPastes(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "scrap",
		"ok":      true,
		"pastes":  n,
	})
}

// ── template helpers ───────────────────────────────────────────────────────

// relAge renders an RFC 3339 timestamp as compact relative age.
func relAge(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// ttlText renders an expiry deadline as a server-side countdown (” = never).
func ttlText(expiresAt string) string {
	if expiresAt == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return ""
	}
	d := time.Until(t)
	switch {
	case d <= 0:
		return "expired"
	case d < time.Hour:
		return fmt.Sprintf("expires in %dm", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("expires in %dh", int(d.Hours())+1)
	default:
		return fmt.Sprintf("expires in %dd", int(d.Hours()/24)+1)
	}
}

// sizeText renders a byte count for the meta line.
func sizeText(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}
