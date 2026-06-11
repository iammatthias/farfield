package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

// Server holds the running QR service.
type Server struct {
	db        *sql.DB
	auth      *web.Auth
	rd        *web.Renderer
	publicURL string // base URL for proxy QR targets, e.g. https://qr.farfield.systems

	// svgCache memoizes rendered SVGs. Encoding is a pure function of
	// (payload, EC) and the CID moves whenever either input changes, so a
	// CID-keyed entry never goes stale — edits simply miss into a new key.
	mu       sync.RWMutex
	svgCache map[string]string
}

// maxSVGCache bounds the memo map. Stale CIDs accumulate as codes are
// edited; past the cap the map is reset rather than evicted piecemeal —
// the next requests simply re-encode.
const maxSVGCache = 1024

// run wires up dependencies and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("QR_DB_PATH", "qr.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	s := &Server{
		db: db,
		auth: &web.Auth{
			DB:           db,
			Password:     store.Env("PASSWORD", ""),
			APIKey:       store.Env("QR_API_KEY", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		publicURL: strings.TrimRight(store.Env("QR_PUBLIC_URL", "http://"+net.JoinHostPort(host, port)), "/"),
	}

	// Templates parse after the Server exists so qrFor can close over s and
	// share the SVG cache with the HTTP handlers.
	tmpl, err := web.ParseTemplates(assets, s.templateFuncs())
	if err != nil {
		return err
	}
	s.rd = &web.Renderer{Templates: tmpl, AssetVer: theme.Version}

	return web.Serve(host, port, s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// HTML admin UI — session-gated.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleIndex))
	mux.HandleFunc("GET /new", s.auth.RequireSession(s.handleNewForm))
	mux.HandleFunc("POST /codes", s.auth.RequireSession(s.handleCreate))
	mux.HandleFunc("GET /codes/{id}/edit", s.auth.RequireSession(s.handleEditForm))
	mux.HandleFunc("POST /codes/{id}", s.auth.RequireSession(s.handleUpdate))
	mux.HandleFunc("POST /codes/{id}/delete", s.auth.RequireSession(s.handleDelete))
	mux.HandleFunc("GET /codes/{id}/preview", s.auth.RequireSession(s.handlePreview))

	// Login.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// Public QR rendering — only for codes marked public AND enabled. The
	// .svg suffix is optional; {id} captures it and the handler trims it.
	mux.HandleFunc("GET /qr/{id}", s.handleQRSVG)
	mux.HandleFunc("GET /r/{id}", s.handleProxyRedirect)

	// Public JSON read API.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/codes", s.handleAPIList)
	mux.HandleFunc("GET /api/codes/{id}", s.handleAPIGet)

	// API-key-gated write API.
	mux.HandleFunc("POST /api/codes", s.auth.RequireAPIKey(s.handleAPICreate))
	mux.HandleFunc("PUT /api/codes/{id}", s.auth.RequireAPIKey(s.handleAPIUpdate))
	mux.HandleFunc("DELETE /api/codes/{id}", s.auth.RequireAPIKey(s.handleAPIDelete))

	// Shared theme stylesheet.
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// Everything qr serves is text — HTML, JSON, SVG — so Gzip wraps the
	// whole mux. Logging sits outside so the recorded status is the final one;
	// pulse traffic recording sits innermost so logged timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(pulse.Middleware(s.db, "qr")(mux))),
		"GET", "POST", "PUT", "DELETE", "OPTIONS")
}

// ── HTML admin handlers ────────────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	cs, err := listCodes(s.db)
	if err != nil {
		s.fail(w, "list codes", err)
		return
	}
	publicN, enabledN := 0, 0
	for _, c := range cs {
		if c.Public {
			publicN++
		}
		if c.Enabled {
			enabledN++
		}
	}
	s.rd.Render(w, "index.html", map[string]any{
		"Codes":     cs,
		"Total":     len(cs),
		"Public":    publicN,
		"Enabled":   enabledN,
		"PublicURL": s.publicURL,
	})
}

func (s *Server) handleNewForm(w http.ResponseWriter, r *http.Request) {
	s.renderForm(w, &Code{
		Mode:    ModeDirect,
		EC:      "M",
		Public:  false,
		Enabled: true,
	}, true, "/codes", "")
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	c := codeFromForm(r)
	if msg := validateCode(c); msg != "" {
		s.renderForm(w, c, true, "/codes", msg)
		return
	}
	if err := insertCode(s.db, c); err != nil {
		s.fail(w, "create code", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleEditForm(w http.ResponseWriter, r *http.Request) {
	c, err := getCode(s.db, r.PathValue("id"))
	if err != nil {
		s.fail(w, "get code", err)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	s.renderForm(w, c, false, "/codes/"+c.ID, "")
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := getCode(s.db, id)
	if err != nil {
		s.fail(w, "get code", err)
		return
	}
	if existing == nil {
		http.NotFound(w, r)
		return
	}
	c := codeFromForm(r)
	c.CreatedAt = existing.CreatedAt
	if msg := validateCode(c); msg != "" {
		c.ID = id
		s.renderForm(w, c, false, "/codes/"+id, msg)
		return
	}
	ok, err := updateCode(s.db, id, c)
	if err != nil {
		s.fail(w, "update code", err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteCode(s.db, r.PathValue("id")); err != nil {
		s.fail(w, "delete code", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handlePreview renders the QR for a single code as an SVG — admin-only,
// available regardless of public/enabled flags so the admin can inspect any
// record without flipping it live.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	c, err := getCode(s.db, r.PathValue("id"))
	if err != nil {
		s.fail(w, "get code", err)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	svg, _, err := s.encodeFor(c)
	if err != nil {
		s.fail(w, "encode QR", err)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = io.WriteString(w, svg)
}

// codeFromForm reads a Code from the admin form. Trimming happens here so
// handlers see a single normalized shape.
func codeFromForm(r *http.Request) *Code {
	_ = r.ParseForm()
	return &Code{
		Label:      r.FormValue("label"),
		Mode:       Mode(strings.TrimSpace(r.FormValue("mode"))),
		Target:     r.FormValue("target"),
		EC:         r.FormValue("ec"),
		Public:     r.FormValue("public") == "on",
		Enabled:    r.FormValue("enabled") == "on",
		AdminNotes: r.FormValue("admin_notes"),
	}
}

// validateCode returns an empty string on success or a human-friendly error
// message suitable for re-rendering the form. Defends against missing target,
// unknown mode, and oversized payloads (the QR encoder caps at version 40).
func validateCode(c *Code) string {
	normalize(c)
	if c.Target == "" {
		return "Target payload is required."
	}
	if !validMode(c.Mode) {
		return "Mode must be 'direct' or 'proxy'."
	}
	ec, ok := ParseECLevel(c.EC)
	if !ok {
		return "Error-correction level must be one of L, M, Q, H."
	}
	// Direct mode encodes the target itself — dry-run the version pick so an
	// oversized payload is rejected here instead of persisting a code that
	// 500s at render time. Proxy payloads are short server URLs; skip them.
	if c.Mode == ModeDirect {
		if _, err := pickVersion(len(c.Target), ec); err != nil {
			return "Target is too large to encode as a QR code at this error-correction level."
		}
	}
	return ""
}

// encodeFor renders the QR SVG for a code, choosing what to encode based on
// Mode. Returns the SVG, the chosen QR version, and any encoder error.
//
// Results are memoized: encoding is pure in (payload, EC) and the full
// pipeline (Reed-Solomon + 8 mask trials) is the most expensive work in the
// service, so repeat renders of the same code hit the cache.
func (s *Server) encodeFor(c *Code) (string, int, error) {
	ec, _ := ParseECLevel(c.EC)
	payload := c.Target
	if c.Mode == ModeProxy {
		payload = s.publicURL + "/r/" + c.ID
	}

	key := svgCacheKey(c)
	if key != "" {
		s.mu.RLock()
		svg, ok := s.svgCache[key]
		s.mu.RUnlock()
		if ok {
			// The version is a cheap pure table lookup — recompute instead of
			// caching a second value alongside the SVG.
			v, err := pickVersion(len(payload), ec)
			return svg, v, err
		}
	}

	svg, v, err := EncodeSVG([]byte(payload), ec)
	if err != nil {
		return "", 0, err
	}
	if key != "" {
		s.mu.Lock()
		if s.svgCache == nil || len(s.svgCache) >= maxSVGCache {
			s.svgCache = make(map[string]string)
		}
		s.svgCache[key] = svg
		s.mu.Unlock()
	}
	return svg, v, nil
}

// svgCacheKey derives the memo key for a code, or "" when the code is not
// cacheable (unsaved form previews have no CID yet). The CID covers every
// encoding input for direct codes; proxy payloads additionally embed the
// record ID (the CID deliberately excludes it), so the ID joins the key to
// keep two same-content proxy codes from sharing an entry.
func svgCacheKey(c *Code) string {
	if c.CID == "" {
		return ""
	}
	if c.Mode == ModeProxy {
		return c.CID + "/" + c.ID
	}
	return c.CID
}

// ── login ──────────────────────────────────────────────────────────────────

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

// ── public QR rendering ────────────────────────────────────────────────────

// handleQRSVG renders the QR SVG for a code that is both public and enabled.
// Private or disabled records return 404 so their existence is not revealed.
// The CID is sent as a strong ETag so clients (and CDNs) can revalidate.
func (s *Server) handleQRSVG(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("id"), ".svg")
	c, err := getCode(s.db, id)
	if err != nil {
		s.fail(w, "get code", err)
		return
	}
	if c == nil || !c.Public || !c.Enabled {
		http.NotFound(w, r)
		return
	}
	svg, _, err := s.encodeFor(c)
	if err != nil {
		s.fail(w, "encode QR", err)
		return
	}
	// For DIRECT codes, the SVG bytes are pinned to the CID — safe to cache
	// long. For PROXY codes the SVG is also pinned to CID (the QR encodes the
	// proxy URL, not the target), but we still revalidate so a flip to
	// private/disabled propagates promptly.
	w.Header().Set("ETag", `"`+c.CID+`"`)
	if web.ETagMatch(r, c.CID) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = io.WriteString(w, svg)
}

// handleProxyRedirect resolves a proxy code's current target and 302s to it.
// Only public+enabled proxy codes resolve; direct codes have no /r/ path.
func (s *Server) handleProxyRedirect(w http.ResponseWriter, r *http.Request) {
	c, err := getCode(s.db, r.PathValue("id"))
	if err != nil {
		s.fail(w, "get code", err)
		return
	}
	if c == nil || c.Mode != ModeProxy || !c.Public || !c.Enabled {
		http.NotFound(w, r)
		return
	}
	if c.Target == "" {
		http.NotFound(w, r)
		return
	}
	// 302 (not 301) — the target can be edited at any time, so caches must not
	// pin the redirect.
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, c.Target, http.StatusFound)
}

// ── public JSON read API ───────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countPublicCodes(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "qr",
		"ok":      true,
		"codes":   n,
	})
}

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	cs, err := listPublicCodes(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list codes")
		return
	}
	public := publicList(cs)
	web.WriteRecord(w, r, cid.OfValue(public), map[string]any{"codes": public})
}

func (s *Server) handleAPIGet(w http.ResponseWriter, r *http.Request) {
	c, err := getCode(s.db, r.PathValue("id"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read code")
		return
	}
	if c == nil || !c.Public || !c.Enabled {
		web.WriteError(w, http.StatusNotFound, "code not found")
		return
	}
	web.WriteRecord(w, r, c.CID, publicView(c))
}

// ── API-key-gated write API ────────────────────────────────────────────────

func (s *Server) handleAPICreate(w http.ResponseWriter, r *http.Request) {
	var c Code
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	c.ID = "" // server-assigned
	if c.Mode == "" {
		c.Mode = ModeDirect
	}
	if c.EC == "" {
		c.EC = "M"
	}
	if msg := validateCode(&c); msg != "" {
		web.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	if err := insertCode(s.db, &c); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not create code")
		return
	}
	web.WriteJSON(w, http.StatusCreated, &c)
}

func (s *Server) handleAPIUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := getCode(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read code")
		return
	}
	if existing == nil {
		web.WriteError(w, http.StatusNotFound, "code not found")
		return
	}
	var c Code
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// Fields not posted fall back to the existing record so partial updates
	// don't wipe metadata.
	if c.Mode == "" {
		c.Mode = existing.Mode
	}
	if c.EC == "" {
		c.EC = existing.EC
	}
	if strings.TrimSpace(c.Target) == "" {
		c.Target = existing.Target
	}
	c.CreatedAt = existing.CreatedAt
	if msg := validateCode(&c); msg != "" {
		web.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	ok, err := updateCode(s.db, id, &c)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not update code")
		return
	}
	if !ok {
		web.WriteError(w, http.StatusNotFound, "code not found")
		return
	}
	web.WriteJSON(w, http.StatusOK, &c)
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	existed, err := deleteCode(s.db, r.PathValue("id"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete code")
		return
	}
	if !existed {
		web.WriteError(w, http.StatusNotFound, "code not found")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}

// ── render helpers ─────────────────────────────────────────────────────────

// templateFuncs returns the FuncMap the qr templates use — qrFor renders a
// code's QR inline as SVG. It is a method so the closure shares the Server's
// SVG cache (and publicURL) with the HTTP handlers via encodeFor.
func (s *Server) templateFuncs() template.FuncMap {
	return template.FuncMap{
		"qrFor": func(c *Code) template.HTML {
			svg, _, err := s.encodeFor(c)
			if err != nil {
				// A visible failure beats an empty figure with no explanation.
				return template.HTML(`<p class="error">QR encode failed: ` +
					template.HTMLEscapeString(err.Error()) + `</p>`)
			}
			return template.HTML(svg)
		},
	}
}

func (s *Server) renderForm(w http.ResponseWriter, c *Code, isNew bool, action, errMsg string) {
	s.rd.Render(w, "code_form.html", map[string]any{
		"Code":      c,
		"IsNew":     isNew,
		"Action":    action,
		"Error":     errMsg,
		"PublicURL": s.publicURL,
	})
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
