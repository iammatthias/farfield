package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
)

//go:embed templates
var assets embed.FS

// Server holds the running QR service.
type Server struct {
	db           *sql.DB
	templates    map[string]*template.Template
	password     string
	apiKey       string
	cookieSecure bool
	publicURL    string // base URL for proxy QR targets, e.g. https://qr.farfield.systems
	assetVer     string
}

// run wires up dependencies and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("QR_DB_PATH", "qr.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	tmpl, err := parseTemplates()
	if err != nil {
		return err
	}

	s := &Server{
		db:           db,
		templates:    tmpl,
		password:     store.Env("PASSWORD", ""),
		apiKey:       store.Env("QR_API_KEY", ""),
		cookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		publicURL:    strings.TrimRight(store.Env("QR_PUBLIC_URL", "http://"+net.JoinHostPort(host, port)), "/"),
		assetVer:     cid.Of([]byte(theme.CSS))[:16],
	}

	srv := &http.Server{Addr: net.JoinHostPort(host, port), Handler: s.routes()}

	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// HTML admin UI — session-gated.
	mux.HandleFunc("GET /{$}", s.requireSession(s.handleIndex))
	mux.HandleFunc("GET /new", s.requireSession(s.handleNewForm))
	mux.HandleFunc("POST /codes", s.requireSession(s.handleCreate))
	mux.HandleFunc("GET /codes/{id}/edit", s.requireSession(s.handleEditForm))
	mux.HandleFunc("POST /codes/{id}", s.requireSession(s.handleUpdate))
	mux.HandleFunc("POST /codes/{id}/delete", s.requireSession(s.handleDelete))
	mux.HandleFunc("GET /codes/{id}/preview", s.requireSession(s.handlePreview))

	// Login.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /logout", s.handleLogout)

	// Public QR rendering — only for codes marked public AND enabled. The
	// .svg suffix is optional; {id} captures it and the handler trims it.
	mux.HandleFunc("GET /qr/{id}", s.handleQRSVG)
	mux.HandleFunc("GET /r/{id}", s.handleProxyRedirect)

	// Public JSON read API.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/codes", s.handleAPIList)
	mux.HandleFunc("GET /api/codes/{id}", s.handleAPIGet)

	// API-key-gated write API.
	mux.HandleFunc("POST /api/codes", s.requireAPIKey(s.handleAPICreate))
	mux.HandleFunc("PUT /api/codes/{id}", s.requireAPIKey(s.handleAPIUpdate))
	mux.HandleFunc("DELETE /api/codes/{id}", s.requireAPIKey(s.handleAPIDelete))

	// Shared theme stylesheet.
	mux.HandleFunc("GET /static/styles.css", handleCSS)

	return cors(logRequests(mux))
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
	s.render(w, "index.html", map[string]any{
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
func (s *Server) encodeFor(c *Code) (string, int, error) {
	ec, _ := ParseECLevel(c.EC)
	payload := c.Target
	if c.Mode == ModeProxy {
		payload = s.publicURL + "/r/" + c.ID
	}
	return EncodeSVG([]byte(payload), ec)
}

// ── login ──────────────────────────────────────────────────────────────────

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if s.password == "" || !auth.VerifyPassword(r.FormValue("password"), s.password) {
		http.Redirect(w, r, "/login?error=Invalid+password", http.StatusSeeOther)
		return
	}
	token := auth.NewSessionToken()
	if err := store.InsertSession(s.db, token, time.Now().Add(7*24*time.Hour)); err != nil {
		s.fail(w, "create session", err)
		return
	}
	http.SetCookie(w, auth.SessionCookie(token, s.cookieSecure))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token, ok := auth.Session(r); ok {
		_ = store.DeleteSession(s.db, token)
	}
	http.SetCookie(w, auth.ClearCookie(s.cookieSecure))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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
	etag := `"` + c.CID + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
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
	cs, err := listPublicCodes(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "qr",
		"ok":      true,
		"codes":   len(cs),
	})
}

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	cs, err := listPublicCodes(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list codes")
		return
	}
	public := publicList(cs)
	etag := `"` + cid.OfValue(public) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"codes": public})
}

func (s *Server) handleAPIGet(w http.ResponseWriter, r *http.Request) {
	c, err := getCode(s.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read code")
		return
	}
	if c == nil || !c.Public || !c.Enabled {
		writeError(w, http.StatusNotFound, "code not found")
		return
	}
	writeRecord(w, r, c.CID, publicView(c))
}

// ── API-key-gated write API ────────────────────────────────────────────────

func (s *Server) handleAPICreate(w http.ResponseWriter, r *http.Request) {
	var c Code
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
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
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err := insertCode(s.db, &c); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create code")
		return
	}
	writeJSON(w, http.StatusCreated, &c)
}

func (s *Server) handleAPIUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := getCode(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read code")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "code not found")
		return
	}
	var c Code
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
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
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	ok, err := updateCode(s.db, id, &c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update code")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "code not found")
		return
	}
	writeJSON(w, http.StatusOK, &c)
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	existed, err := deleteCode(s.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete code")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "code not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}

// ── render helpers ─────────────────────────────────────────────────────────

func (s *Server) renderForm(w http.ResponseWriter, c *Code, isNew bool, action, errMsg string) {
	s.render(w, "code_form.html", map[string]any{
		"Code":      c,
		"IsNew":     isNew,
		"Action":    action,
		"Error":     errMsg,
		"PublicURL": s.publicURL,
	})
}

func parseTemplates() (map[string]*template.Template, error) {
	pages, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	funcs := template.FuncMap{
		"qrFor": func(c *Code, publicURL string) template.HTML {
			payload := c.Target
			if c.Mode == ModeProxy {
				payload = strings.TrimRight(publicURL, "/") + "/r/" + c.ID
			}
			ec, _ := ParseECLevel(c.EC)
			svg, _, err := EncodeSVG([]byte(payload), ec)
			if err != nil {
				// A visible failure beats an empty figure with no explanation.
				return template.HTML(`<p class="error">QR encode failed: ` +
					template.HTMLEscapeString(err.Error()) + `</p>`)
			}
			return template.HTML(svg)
		},
	}
	out := make(map[string]*template.Template)
	for _, page := range pages {
		name := path.Base(page)
		if name == "base.html" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(assets, "templates/base.html", page)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		out[name] = t
	}
	return out, nil
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	t, ok := s.templates[page]
	if !ok {
		slog.Error("unknown template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if m, ok := data.(map[string]any); ok {
		m["AssetVer"] = s.assetVer
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		slog.Error("render failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// writeRecord writes v with its CID as the strong ETag, and short-circuits to
// 304 when the client already holds that version.
func writeRecord(w http.ResponseWriter, r *http.Request, recCID string, v any) {
	etag := `"` + recCID + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(w, theme.CSS)
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request",
			"method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
	})
}
