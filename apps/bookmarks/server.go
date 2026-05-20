package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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

// Server holds the running bookmarks service.
type Server struct {
	db           *sql.DB
	templates    map[string]*template.Template
	password     string
	apiKey       string
	cookieSecure bool
	http         *http.Client
	assetVer     string
}

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("BOOKMARKS_DB_PATH", "bookmarks.sqlite"))
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
		apiKey:       store.Env("BOOKMARKS_API_KEY", ""),
		cookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		http:         &http.Client{Timeout: fetchTimeout + 5*time.Second},
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

	// Public HTML.
	mux.HandleFunc("GET /{$}", s.handlePublicIndex)

	// Admin HTML — session-gated, mounted under /admin.
	mux.HandleFunc("GET /admin", s.requireSession(s.handleAdminIndex))
	mux.HandleFunc("GET /admin/{$}", s.requireSession(s.handleAdminIndex))
	mux.HandleFunc("GET /admin/new", s.requireSession(s.handleNewForm))
	mux.HandleFunc("POST /admin/bookmarks", s.requireSession(s.handleCreate))
	mux.HandleFunc("GET /admin/bookmarks/{id}/edit", s.requireSession(s.handleEditForm))
	mux.HandleFunc("POST /admin/bookmarks/{id}", s.requireSession(s.handleUpdate))
	mux.HandleFunc("POST /admin/bookmarks/{id}/delete", s.requireSession(s.handleDelete))
	mux.HandleFunc("POST /admin/bookmarks/{id}/refetch", s.requireSession(s.handleRefetch))

	// Admin login.
	mux.HandleFunc("GET /admin/login", s.handleLoginForm)
	mux.HandleFunc("POST /admin/login", s.handleLogin)
	mux.HandleFunc("GET /admin/logout", s.handleLogout)

	// Public JSON read API.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/bookmarks", s.handleAPIList)
	mux.HandleFunc("GET /api/bookmarks/{id}", s.handleAPIGet)

	// API-key-gated write API.
	mux.HandleFunc("POST /api/bookmarks", s.requireAPIKey(s.handleAPICreate))
	mux.HandleFunc("PUT /api/bookmarks/{id}", s.requireAPIKey(s.handleAPIUpdate))
	mux.HandleFunc("DELETE /api/bookmarks/{id}", s.requireAPIKey(s.handleAPIDelete))

	// Shared theme stylesheet.
	mux.HandleFunc("GET /static/styles.css", handleCSS)

	return cors(logRequests(mux))
}

// ── public HTML ─────────────────────────────────────────────────────────────

func (s *Server) handlePublicIndex(w http.ResponseWriter, r *http.Request) {
	bs, err := listPublicBookmarks(s.db)
	if err != nil {
		s.fail(w, "list public bookmarks", err)
		return
	}
	groups := groupByCategory(publicList(bs))
	s.render(w, "public_index.html", map[string]any{
		"Groups": groups,
		"Total":  len(bs),
	})
}

// ── admin HTML ──────────────────────────────────────────────────────────────

func (s *Server) handleAdminIndex(w http.ResponseWriter, r *http.Request) {
	bs, err := listBookmarks(s.db)
	if err != nil {
		s.fail(w, "list bookmarks", err)
		return
	}
	publicN := 0
	for _, b := range bs {
		if b.Public {
			publicN++
		}
	}
	s.render(w, "admin_index.html", map[string]any{
		"Bookmarks": bs,
		"Total":     len(bs),
		"Public":    publicN,
		"Private":   len(bs) - publicN,
	})
}

func (s *Server) handleNewForm(w http.ResponseWriter, r *http.Request) {
	s.renderForm(w, &Bookmark{Public: true}, true, "/admin/bookmarks", "")
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	b := bookmarkFromForm(r)
	if b.URL == "" {
		s.renderForm(w, b, true, "/admin/bookmarks", "URL is required.")
		return
	}
	if _, err := url.ParseRequestURI(b.URL); err != nil {
		s.renderForm(w, b, true, "/admin/bookmarks", "URL is malformed.")
		return
	}
	s.fetchAndApply(r.Context(), b)
	if err := insertBookmark(s.db, b); err != nil {
		s.fail(w, "create bookmark", err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleEditForm(w http.ResponseWriter, r *http.Request) {
	b, err := getBookmark(s.db, r.PathValue("id"))
	if err != nil {
		s.fail(w, "get bookmark", err)
		return
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	s.renderForm(w, b, false, "/admin/bookmarks/"+b.ID, "")
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := getBookmark(s.db, id)
	if err != nil {
		s.fail(w, "get bookmark", err)
		return
	}
	if existing == nil {
		http.NotFound(w, r)
		return
	}
	b := bookmarkFromForm(r)
	// Preserve fetched fields and creation timestamp — the edit form does not
	// post these. A refetch is the explicit way to refresh them.
	b.OGTitle = existing.OGTitle
	b.OGDescription = existing.OGDescription
	b.OGImage = existing.OGImage
	b.OGSiteName = existing.OGSiteName
	b.OGType = existing.OGType
	b.MetaAuthor = existing.MetaAuthor
	b.Favicon = existing.Favicon
	b.CreatedAt = existing.CreatedAt
	if b.URL == "" {
		b.ID = id
		s.renderForm(w, b, false, "/admin/bookmarks/"+id, "URL is required.")
		return
	}
	ok, err := updateBookmark(s.db, id, b)
	if err != nil {
		s.fail(w, "update bookmark", err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteBookmark(s.db, r.PathValue("id")); err != nil {
		s.fail(w, "delete bookmark", err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleRefetch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := getBookmark(s.db, id)
	if err != nil {
		s.fail(w, "get bookmark", err)
		return
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	// Refetch refreshes OG/meta fields even when admin-overridden ones are
	// already set — that is the point of the action. Clear them so the
	// extractor's fresh values land verbatim.
	b.OGTitle, b.OGDescription, b.OGImage = "", "", ""
	b.OGSiteName, b.OGType, b.MetaAuthor, b.Favicon = "", "", "", ""
	s.fetchAndApply(r.Context(), b)
	if _, err := updateBookmark(s.db, id, b); err != nil {
		s.fail(w, "update bookmark", err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// fetchAndApply fetches the bookmark's URL and applies the extracted metadata.
// A failure is logged but never surfaced to the caller — the bookmark must be
// savable even when the remote host is offline.
func (s *Server) fetchAndApply(ctx context.Context, b *Bookmark) {
	meta, err := fetchMetadata(ctx, s.http, b.URL)
	if err != nil {
		slog.Warn("metadata fetch failed", "url", b.URL, "err", err)
	}
	applyMetadata(b, meta)
}

// bookmarkFromForm reads a Bookmark from a posted admin form. All trimming
// happens here so handlers see consistently shaped values.
func bookmarkFromForm(r *http.Request) *Bookmark {
	_ = r.ParseForm()
	return &Bookmark{
		URL:         strings.TrimSpace(r.FormValue("url")),
		Title:       strings.TrimSpace(r.FormValue("title")),
		Description: strings.TrimSpace(r.FormValue("description")),
		Category:    strings.TrimSpace(r.FormValue("category")),
		Public:      r.FormValue("public") == "on",
		AdminNotes:  strings.TrimSpace(r.FormValue("admin_notes")),
	}
}

// ── login ───────────────────────────────────────────────────────────────────

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if s.password == "" || !auth.VerifyPassword(r.FormValue("password"), s.password) {
		http.Redirect(w, r, "/admin/login?error=Invalid+password", http.StatusSeeOther)
		return
	}
	token := auth.NewSessionToken()
	if err := store.InsertSession(s.db, token, time.Now().Add(7*24*time.Hour)); err != nil {
		s.fail(w, "create session", err)
		return
	}
	http.SetCookie(w, auth.SessionCookie(token, s.cookieSecure))
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token, ok := auth.Session(r); ok {
		_ = store.DeleteSession(s.db, token)
	}
	http.SetCookie(w, auth.ClearCookie(s.cookieSecure))
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// ── public JSON read API ────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	bs, err := listPublicBookmarks(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":   "bookmarks",
		"ok":        true,
		"bookmarks": len(bs),
	})
}

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	bs, err := listPublicBookmarks(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list bookmarks")
		return
	}
	public := publicList(bs)
	etag := `"` + cid.OfValue(public) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bookmarks": public})
}

func (s *Server) handleAPIGet(w http.ResponseWriter, r *http.Request) {
	b, err := getBookmark(s.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read bookmark")
		return
	}
	if b == nil || !b.Public {
		writeError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	writeRecord(w, r, b.CID, publicView(b))
}

// ── API-key-gated write API ────────────────────────────────────────────────

func (s *Server) handleAPICreate(w http.ResponseWriter, r *http.Request) {
	var b Bookmark
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(b.URL) == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}
	b.ID = "" // server-assigned
	s.fetchAndApply(r.Context(), &b)
	if err := insertBookmark(s.db, &b); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create bookmark")
		return
	}
	writeJSON(w, http.StatusCreated, &b)
}

func (s *Server) handleAPIUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := getBookmark(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read bookmark")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	var b Bookmark
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(b.URL) == "" {
		b.URL = existing.URL
	}
	b.CreatedAt = existing.CreatedAt
	ok, err := updateBookmark(s.db, id, &b)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update bookmark")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	writeJSON(w, http.StatusOK, &b)
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	existed, err := deleteBookmark(s.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete bookmark")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}

// ── render helpers ─────────────────────────────────────────────────────────

func (s *Server) renderForm(w http.ResponseWriter, b *Bookmark, isNew bool, action, errMsg string) {
	s.render(w, "bookmark_form.html", map[string]any{
		"Bookmark": b, "IsNew": isNew, "Action": action, "Error": errMsg,
	})
}

func parseTemplates() (map[string]*template.Template, error) {
	pages, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	out := make(map[string]*template.Template)
	for _, page := range pages {
		name := path.Base(page)
		if name == "base.html" {
			continue
		}
		t, err := template.ParseFS(assets, "templates/base.html", page)
		if err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
}

// render writes a page through base.html, buffering first so a template error
// never produces a half-written response.
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

// writeRecord writes v as JSON with its content CID as the ETag, and
// short-circuits to 304 Not Modified when the client already holds that
// version (If-None-Match).
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

// cors adds permissive CORS headers so a browser on another origin (the
// website) can read the public API, and answers preflight requests.
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
