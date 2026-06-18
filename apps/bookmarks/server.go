package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

// Server holds the running bookmarks service.
type Server struct {
	db   *sql.DB
	auth *web.Auth
	rd   *web.Renderer
	http *http.Client
}

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("BOOKMARKS_DB_PATH", "bookmarks.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	tmpl, err := web.ParseTemplates(assets, nil)
	if err != nil {
		return err
	}

	s := &Server{
		db: db,
		auth: &web.Auth{
			DB:           db,
			Password:     store.Env("PASSWORD", ""),
			APIKey:       store.Env("BOOKMARKS_API_KEY", ""),
			ReadKey:      store.Env("BOOKMARKS_READ_KEY", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd:   &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
		http: &http.Client{Timeout: fetchTimeout + 5*time.Second},
	}

	return web.Serve(host, port, s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// HTML admin UI — session-gated.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleIndex))
	mux.HandleFunc("GET /new", s.auth.RequireSession(s.handleNewForm))
	mux.HandleFunc("POST /bookmarks", s.auth.RequireSession(s.handleCreate))
	mux.HandleFunc("GET /bookmarks/{id}/edit", s.auth.RequireSession(s.handleEditForm))
	mux.HandleFunc("POST /bookmarks/{id}", s.auth.RequireSession(s.handleUpdate))
	mux.HandleFunc("POST /bookmarks/{id}/delete", s.auth.RequireSession(s.handleDelete))
	mux.HandleFunc("POST /bookmarks/{id}/refetch", s.auth.RequireSession(s.handleRefetch))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// JSON read API — bearer-token-gated when BOOKMARKS_READ_KEY is set (the
	// write BOOKMARKS_API_KEY is also accepted). /status stays public.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/bookmarks", s.auth.RequireReadKey(s.handleAPIList))
	mux.HandleFunc("GET /api/bookmarks/{id}", s.auth.RequireReadKey(s.handleAPIGet))
	mux.HandleFunc("GET /api/categories", s.auth.RequireReadKey(s.handleAPICategories))

	// API-key-gated write API.
	mux.HandleFunc("POST /api/bookmarks", s.auth.RequireAPIKey(s.handleAPICreate))
	mux.HandleFunc("PUT /api/bookmarks/{id}", s.auth.RequireAPIKey(s.handleAPIUpdate))
	mux.HandleFunc("DELETE /api/bookmarks/{id}", s.auth.RequireAPIKey(s.handleAPIDelete))

	// Shared theme stylesheet.
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// Everything bookmarks serves is text — HTML, JSON — so Gzip wraps the
	// whole mux. Logging sits outside so the recorded status is the final one;
	// pulse traffic recording sits innermost so logged timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(pulse.Middleware(s.db, "bookmarks")(mux))),
		"GET", "POST", "PUT", "DELETE", "OPTIONS")
}

// ── HTML admin handlers ────────────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
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
	s.rd.Render(w, "index.html", map[string]any{
		"Bookmarks": bs,
		"Total":     len(bs),
		"Public":    publicN,
		"Private":   len(bs) - publicN,
	})
}

func (s *Server) handleNewForm(w http.ResponseWriter, r *http.Request) {
	s.renderForm(w, &Bookmark{Public: true}, true, "/bookmarks", "")
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	b := bookmarkFromForm(r)
	if b.URL == "" {
		s.renderForm(w, b, true, "/bookmarks", "URL is required.")
		return
	}
	if _, err := url.ParseRequestURI(b.URL); err != nil {
		s.renderForm(w, b, true, "/bookmarks", "URL is malformed.")
		return
	}
	if err := insertBookmark(s.db, b); err != nil {
		s.fail(w, "create bookmark", err)
		return
	}
	s.fetchInBackground(*b)
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
	s.renderForm(w, b, false, "/bookmarks/"+b.ID, "")
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
		s.renderForm(w, b, false, "/bookmarks/"+id, "URL is required.")
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
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteBookmark(s.db, r.PathValue("id")); err != nil {
		s.fail(w, "delete bookmark", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
	http.Redirect(w, r, "/", http.StatusSeeOther)
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

// fetchInBackground fetches metadata for a just-saved bookmark in a goroutine,
// so the create path responds immediately instead of blocking on a remote GET
// for up to fetchTimeout (the iOS Shortcut share path felt every second of
// that). b is a copy — the handler's *Bookmark is never touched after the
// response, so there is no data race. When the fetch lands, only the metadata
// fields are merged into the stored row by ID (see updateBookmarkMetadata), so
// an admin edit that raced the fetch is never clobbered. Failures are logged
// and the bookmark simply keeps its bare URL.
func (s *Server) fetchInBackground(b Bookmark) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		meta, err := fetchMetadata(ctx, s.http, b.URL)
		if err != nil {
			slog.Warn("background metadata fetch failed", "id", b.ID, "url", b.URL, "err", err)
			return
		}
		if err := updateBookmarkMetadata(s.db, b.ID, meta); err != nil {
			slog.Warn("could not store fetched metadata", "id", b.ID, "err", err)
		}
	}()
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

// ── login ──────────────────────────────────────────────────────────────────

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

// ── public JSON read API ───────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countPublicBookmarks(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service":   "bookmarks",
		"ok":        true,
		"bookmarks": n,
	})
}

// listETag computes the conditional-GET validator for a public list endpoint
// from the cheap COUNT/MAX(updated_at) version, salted per endpoint so the
// two list resources never share a tag. Returns ok=false (and writes the
// error) when the version query fails. When the client's If-None-Match
// matches, the 304 is written here and ok is false — the caller is done. On
// ok=true the ETag and Cache-Control headers are already set; the caller
// scans, marshals once, and writes the bytes.
func (s *Server) listETag(w http.ResponseWriter, r *http.Request, salt string) bool {
	ver, err := publicBookmarksVersion(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return false
	}
	etag := cid.Of([]byte(salt + ":" + ver))
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	if web.ETagMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return false
	}
	return true
}

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	// Answer 304 from the cheap version probe before scanning any rows.
	if !s.listETag(w, r, "bookmarks") {
		return
	}
	bs, err := listPublicBookmarks(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list bookmarks")
		return
	}
	body, err := json.Marshal(map[string]any{"bookmarks": publicList(bs)})
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not encode bookmarks")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) handleAPIGet(w http.ResponseWriter, r *http.Request) {
	b, err := getBookmark(s.db, r.PathValue("id"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read bookmark")
		return
	}
	if b == nil || !b.Public {
		web.WriteError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	web.WriteRecord(w, r, b.CID, publicView(b))
}

// handleAPICategories returns the public bookmarks grouped by category, in the
// same order the public listing uses (categories first-seen, "Uncategorized"
// last). Useful to clients that want to render a sectioned list without
// re-sorting client-side.
func (s *Server) handleAPICategories(w http.ResponseWriter, r *http.Request) {
	// Answer 304 from the cheap version probe before scanning any rows.
	if !s.listETag(w, r, "categories") {
		return
	}
	bs, err := listPublicBookmarks(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list bookmarks")
		return
	}
	groups := groupByCategory(publicList(bs))
	type group struct {
		Name      string     `json:"name"`
		Bookmarks []Bookmark `json:"bookmarks"`
	}
	out := make([]group, 0, len(groups))
	for _, g := range groups {
		out = append(out, group{Name: g.Name, Bookmarks: g.Bookmarks})
	}
	body, err := json.Marshal(map[string]any{"categories": out})
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not encode categories")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// ── API-key-gated write API ────────────────────────────────────────────────

func (s *Server) handleAPICreate(w http.ResponseWriter, r *http.Request) {
	var b Bookmark
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(b.URL) == "" {
		web.WriteError(w, http.StatusBadRequest, "url is required")
		return
	}
	b.ID = "" // server-assigned
	if err := insertBookmark(s.db, &b); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not create bookmark")
		return
	}
	// Metadata is fetched after the response, so the 201 returns the bare
	// record without OG fields — the trade-off for an instant save (the iOS
	// Shortcut path used to block on a remote GET for up to 10s). Clients
	// that need the enriched record re-GET it by ID once the fetch lands.
	s.fetchInBackground(b)
	web.WriteJSON(w, http.StatusCreated, &b)
}

func (s *Server) handleAPIUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := getBookmark(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read bookmark")
		return
	}
	if existing == nil {
		web.WriteError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	// Decode over a copy of the existing record so a partial body updates
	// only the fields it names — fetched OG metadata, favicon, category,
	// and flags survive instead of being zeroed.
	b := *existing
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(b.URL) == "" {
		b.URL = existing.URL
	}
	b.ID = existing.ID
	b.CreatedAt = existing.CreatedAt
	ok, err := updateBookmark(s.db, id, &b)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not update bookmark")
		return
	}
	if !ok {
		web.WriteError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	web.WriteJSON(w, http.StatusOK, &b)
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	existed, err := deleteBookmark(s.db, r.PathValue("id"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete bookmark")
		return
	}
	if !existed {
		web.WriteError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
}

// ── render helpers ─────────────────────────────────────────────────────────

func (s *Server) renderForm(w http.ResponseWriter, b *Bookmark, isNew bool, action, errMsg string) {
	s.rd.Render(w, "bookmark_form.html", map[string]any{
		"Bookmark": b, "IsNew": isNew, "Action": action, "Error": errMsg,
	})
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
