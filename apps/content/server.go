package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
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
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
)

//go:embed templates
var assets embed.FS

// Server holds the running content service.
type Server struct {
	db           *sql.DB
	templates    map[string]*template.Template
	password     string
	apiKey       string
	cookieSecure bool
}

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("CONTENT_DB_PATH", "content.sqlite"))
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
		apiKey:       store.Env("CONTENT_API_KEY", ""),
		cookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
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
	mux.HandleFunc("GET /{$}", s.requireSession(s.handleDashboard))
	mux.HandleFunc("GET /collections/new", s.requireSession(s.handleNewCollection))
	mux.HandleFunc("POST /collections", s.requireSession(s.handleCreateCollection))
	mux.HandleFunc("GET /collections/{slug}/edit", s.requireSession(s.handleEditCollection))
	mux.HandleFunc("POST /collections/{slug}", s.requireSession(s.handleUpdateCollection))
	mux.HandleFunc("POST /collections/{slug}/delete", s.requireSession(s.handleDeleteCollection))
	mux.HandleFunc("GET /entries", s.requireSession(s.handleEntries))
	mux.HandleFunc("GET /entries/new", s.requireSession(s.handleNewEntry))
	mux.HandleFunc("POST /entries", s.requireSession(s.handleCreateEntry))
	mux.HandleFunc("GET /entries/{slug}/edit", s.requireSession(s.handleEditEntry))
	mux.HandleFunc("POST /entries/{slug}", s.requireSession(s.handleUpdateEntry))
	mux.HandleFunc("POST /entries/{slug}/delete", s.requireSession(s.handleDeleteEntry))
	mux.HandleFunc("GET /series", s.requireSession(s.handleSeriesList))
	mux.HandleFunc("GET /series/new", s.requireSession(s.handleNewSeries))
	mux.HandleFunc("POST /series", s.requireSession(s.handleCreateSeries))
	mux.HandleFunc("GET /series/{rkey}/edit", s.requireSession(s.handleEditSeries))
	mux.HandleFunc("POST /series/{rkey}", s.requireSession(s.handleUpdateSeries))
	mux.HandleFunc("POST /series/{rkey}/delete", s.requireSession(s.handleDeleteSeries))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /logout", s.handleLogout)

	// Public JSON read API — published content only.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/collections", s.handleAPICollections)
	mux.HandleFunc("GET /api/entries", s.handleAPIEntries)
	mux.HandleFunc("GET /api/entries/{slug}", s.handleAPIEntry)
	mux.HandleFunc("GET /api/series", s.handleAPISeries)
	mux.HandleFunc("GET /api/series/{rkey}", s.handleAPISeriesOne)

	// JSON write API — API-key-gated.
	mux.HandleFunc("POST /api/entries", s.requireAPIKey(s.handleAPICreateEntry))
	mux.HandleFunc("PUT /api/entries/{slug}", s.requireAPIKey(s.handleAPIUpdateEntry))
	mux.HandleFunc("DELETE /api/entries/{slug}", s.requireAPIKey(s.handleAPIDeleteEntry))

	// Shared theme stylesheet.
	mux.HandleFunc("GET /static/styles.css", handleCSS)

	return cors(logRequests(mux))
}

// ── HTML admin: dashboard ──────────────────────────────────────────────────

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	entries, err := listEntries(s.db, "", false)
	if err != nil {
		s.fail(w, "list entries", err)
		return
	}
	recent := entries
	if len(recent) > 12 {
		recent = recent[:12]
	}
	s.render(w, "dashboard.html", map[string]any{
		"Collections": collections,
		"Entries":     recent,
		"TotalCount":  len(entries),
	})
}

// ── HTML admin: collections ────────────────────────────────────────────────

func (s *Server) handleNewCollection(w http.ResponseWriter, r *http.Request) {
	s.render(w, "collection_form.html", map[string]any{
		"IsNew": true, "Action": "/collections", "Collection": Collection{},
	})
}

func (s *Server) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	c := &Collection{
		Name:        strings.TrimSpace(r.FormValue("name")),
		Slug:        firstNonEmpty(slugify(r.FormValue("slug")), slugify(r.FormValue("name"))),
		Description: strings.TrimSpace(r.FormValue("description")),
	}
	if c.Name == "" || c.Slug == "" {
		s.renderCollectionForm(w, c, true, "/collections", "Name is required.")
		return
	}
	if err := insertCollection(s.db, c); err != nil {
		if errors.Is(err, errSlugTaken) {
			s.renderCollectionForm(w, c, true, "/collections", err.Error())
			return
		}
		s.fail(w, "create collection", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleEditCollection(w http.ResponseWriter, r *http.Request) {
	c, err := getCollection(s.db, r.PathValue("slug"))
	if err != nil {
		s.fail(w, "get collection", err)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	s.renderCollectionForm(w, c, false, "/collections/"+c.Slug, "")
}

func (s *Server) handleUpdateCollection(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	desc := strings.TrimSpace(r.FormValue("description"))
	if name == "" {
		c := &Collection{Slug: slug, Name: name, Description: desc}
		s.renderCollectionForm(w, c, false, "/collections/"+slug, "Name is required.")
		return
	}
	ok, err := updateCollection(s.db, slug, name, desc)
	if err != nil {
		s.fail(w, "update collection", err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDeleteCollection(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteCollection(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "delete collection", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ── HTML admin: entries ────────────────────────────────────────────────────

func (s *Server) handleEntries(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("collection")
	entries, err := listEntries(s.db, filter, false)
	if err != nil {
		s.fail(w, "list entries", err)
		return
	}
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	s.render(w, "entries.html", map[string]any{
		"Entries": entries, "Collections": collections, "Filter": filter,
	})
}

func (s *Server) handleNewEntry(w http.ResponseWriter, r *http.Request) {
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	if len(collections) == 0 {
		http.Redirect(w, r, "/collections/new", http.StatusSeeOther)
		return
	}
	s.renderEntryForm(w, &Entry{Published: false}, collections, true, "/entries", "")
}

func (s *Server) handleCreateEntry(w http.ResponseWriter, r *http.Request) {
	e := entryFromForm(r)
	if e.Title == "" || e.Slug == "" || e.Collection == "" {
		s.reRenderEntryForm(w, e, true, "/entries", "Title and collection are required.")
		return
	}
	if err := insertEntry(s.db, e); err != nil {
		s.reRenderEntryForm(w, e, true, "/entries", err.Error())
		return
	}
	http.Redirect(w, r, "/entries", http.StatusSeeOther)
}

func (s *Server) handleEditEntry(w http.ResponseWriter, r *http.Request) {
	e, err := getEntry(s.db, r.PathValue("slug"))
	if err != nil {
		s.fail(w, "get entry", err)
		return
	}
	if e == nil {
		http.NotFound(w, r)
		return
	}
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	s.renderEntryForm(w, e, collections, false, "/entries/"+e.Slug, "")
}

func (s *Server) handleUpdateEntry(w http.ResponseWriter, r *http.Request) {
	current := r.PathValue("slug")
	e := entryFromForm(r)
	if e.Title == "" || e.Slug == "" || e.Collection == "" {
		s.reRenderEntryForm(w, e, false, "/entries/"+current, "Title and collection are required.")
		return
	}
	if err := updateEntry(s.db, current, e); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.reRenderEntryForm(w, e, false, "/entries/"+current, err.Error())
		return
	}
	http.Redirect(w, r, "/entries", http.StatusSeeOther)
}

func (s *Server) handleDeleteEntry(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteEntry(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "delete entry", err)
		return
	}
	http.Redirect(w, r, "/entries", http.StatusSeeOther)
}

// entryFromForm reads an Entry from a posted admin form.
func entryFromForm(r *http.Request) *Entry {
	_ = r.ParseForm()
	title := strings.TrimSpace(r.FormValue("title"))
	slug := firstNonEmpty(slugify(r.FormValue("slug")), slugify(title))
	return &Entry{
		Collection: r.FormValue("collection"),
		Slug:       slug,
		Title:      title,
		Excerpt:    strings.TrimSpace(r.FormValue("excerpt")),
		Body:       r.FormValue("body"),
		Tags:       splitTags(r.FormValue("tags")),
		Published:  r.FormValue("published") != "",
	}
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

// ── public JSON read API ───────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	collections, err := listCollections(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "content", "ok": true, "collections": len(collections),
	})
}

func (s *Server) handleAPICollections(w http.ResponseWriter, r *http.Request) {
	collections, err := listCollections(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list collections")
		return
	}
	if collections == nil {
		collections = []Collection{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"collections": collections})
}

func (s *Server) handleAPIEntries(w http.ResponseWriter, r *http.Request) {
	entries, err := listEntries(s.db, r.URL.Query().Get("collection"), true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list entries")
		return
	}
	if entries == nil {
		entries = []Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleAPIEntry(w http.ResponseWriter, r *http.Request) {
	e, err := getEntry(s.db, r.PathValue("slug"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read entry")
		return
	}
	if e == nil || !e.Published {
		writeError(w, http.StatusNotFound, "entry not found")
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// ── API-key-gated write API ────────────────────────────────────────────────

func (s *Server) handleAPICreateEntry(w http.ResponseWriter, r *http.Request) {
	var e Entry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if e.Slug == "" {
		e.Slug = slugify(e.Title)
	}
	if e.Title == "" || e.Slug == "" || e.Collection == "" {
		writeError(w, http.StatusBadRequest, "title, slug, and collection are required")
		return
	}
	if err := insertEntry(s.db, &e); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (s *Server) handleAPIUpdateEntry(w http.ResponseWriter, r *http.Request) {
	current := r.PathValue("slug")
	var e Entry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if e.Slug == "" {
		e.Slug = current
	}
	if err := updateEntry(s.db, current, &e); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "entry not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) handleAPIDeleteEntry(w http.ResponseWriter, r *http.Request) {
	existed, err := deleteEntry(s.db, r.PathValue("slug"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete entry")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "entry not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("slug")})
}

// ── rendering helpers ──────────────────────────────────────────────────────

func (s *Server) renderCollectionForm(w http.ResponseWriter, c *Collection, isNew bool, action, errMsg string) {
	s.render(w, "collection_form.html", map[string]any{
		"Collection": c, "IsNew": isNew, "Action": action, "Error": errMsg,
	})
}

func (s *Server) renderEntryForm(w http.ResponseWriter, e *Entry, collections []Collection, isNew bool, action, errMsg string) {
	s.render(w, "entry_form.html", map[string]any{
		"Entry": e, "Collections": collections, "IsNew": isNew,
		"Action": action, "Error": errMsg, "TagsText": strings.Join(e.Tags, ", "),
	})
}

// reRenderEntryForm re-shows the entry form after a failed submit.
func (s *Server) reRenderEntryForm(w http.ResponseWriter, e *Entry, isNew bool, action, errMsg string) {
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	s.renderEntryForm(w, e, collections, isNew, action, errMsg)
}

func handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(w, theme.CSS)
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
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		slog.Error("render failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// fail logs an internal error and returns a 500.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ── series: HTML admin ─────────────────────────────────────────────────────

func (s *Server) handleSeriesList(w http.ResponseWriter, r *http.Request) {
	series, err := listSeries(s.db)
	if err != nil {
		s.fail(w, "list series", err)
		return
	}
	s.render(w, "series.html", map[string]any{"Series": series})
}

func (s *Server) handleNewSeries(w http.ResponseWriter, r *http.Request) {
	s.renderSeriesForm(w, &Series{}, true, "/series", "")
}

func (s *Server) handleCreateSeries(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	title := strings.TrimSpace(r.FormValue("title"))
	se := &Series{
		Rkey:  slugify(firstNonEmpty(r.FormValue("rkey"), title)),
		Title: title,
		Body:  r.FormValue("body"),
	}
	if se.Rkey == "" {
		s.renderSeriesForm(w, se, true, "/series", "A series needs an rkey or a title.")
		return
	}
	if existing, _ := getSeries(s.db, se.Rkey); existing != nil {
		s.renderSeriesForm(w, se, true, "/series", "That rkey is already taken.")
		return
	}
	now := nowRFC3339()
	se.CreatedAt, se.UpdatedAt = now, now
	if err := upsertSeries(s.db, se); err != nil {
		s.fail(w, "create series", err)
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

func (s *Server) handleEditSeries(w http.ResponseWriter, r *http.Request) {
	se, err := getSeries(s.db, r.PathValue("rkey"))
	if err != nil {
		s.fail(w, "get series", err)
		return
	}
	if se == nil {
		http.NotFound(w, r)
		return
	}
	s.renderSeriesForm(w, se, false, "/series/"+se.Rkey, "")
}

func (s *Server) handleUpdateSeries(w http.ResponseWriter, r *http.Request) {
	se, err := getSeries(s.db, r.PathValue("rkey"))
	if err != nil {
		s.fail(w, "get series", err)
		return
	}
	if se == nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	se.Title = strings.TrimSpace(r.FormValue("title"))
	se.Body = r.FormValue("body")
	se.UpdatedAt = nowRFC3339()
	if err := upsertSeries(s.db, se); err != nil {
		s.fail(w, "update series", err)
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

func (s *Server) handleDeleteSeries(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteSeries(s.db, r.PathValue("rkey")); err != nil {
		s.fail(w, "delete series", err)
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

func (s *Server) renderSeriesForm(w http.ResponseWriter, se *Series, isNew bool, action, errMsg string) {
	s.render(w, "series_form.html", map[string]any{
		"Series": se, "IsNew": isNew, "Action": action, "Error": errMsg,
	})
}

// ── series: public JSON ────────────────────────────────────────────────────

func (s *Server) handleAPISeries(w http.ResponseWriter, r *http.Request) {
	series, err := listSeries(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list series")
		return
	}
	if series == nil {
		series = []Series{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series})
}

func (s *Server) handleAPISeriesOne(w http.ResponseWriter, r *http.Request) {
	se, err := getSeries(s.db, r.PathValue("rkey"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read series")
		return
	}
	if se == nil {
		writeError(w, http.StatusNotFound, "series not found")
		return
	}
	writeJSON(w, http.StatusOK, se)
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
