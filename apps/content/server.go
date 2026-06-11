package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

// Server holds the running content service.
type Server struct {
	db            *sql.DB
	auth          *web.Auth
	rd            *web.Renderer
	blobsURL      string // internal blobs service URL — for the upload proxy
	blobsKey      string // blobs API key — kept server-side
	blobsPublic   string // browser-facing blobs URL — injected into the editor
	contentPublic string // browser-facing content URL — injected into the editor
}

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("CONTENT_DB_PATH", "content.sqlite"))
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
			APIKey:       store.Env("CONTENT_API_KEY", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd:            &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
		blobsURL:      store.Env("BLOBS_URL", "http://127.0.0.1:8789"),
		blobsKey:      store.Env("BLOBS_API_KEY", ""),
		blobsPublic:   store.Env("BLOBS_PUBLIC_URL", "http://127.0.0.1:8789"),
		contentPublic: store.Env("CONTENT_PUBLIC_URL", "http://127.0.0.1:8787"),
	}

	return web.Serve(host, port, s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// HTML admin UI — session-gated.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleDashboard))
	mux.HandleFunc("GET /collections/new", s.auth.RequireSession(s.handleNewCollection))
	mux.HandleFunc("POST /collections", s.auth.RequireSession(s.handleCreateCollection))
	mux.HandleFunc("GET /collections/{slug}/edit", s.auth.RequireSession(s.handleEditCollection))
	mux.HandleFunc("POST /collections/{slug}", s.auth.RequireSession(s.handleUpdateCollection))
	mux.HandleFunc("POST /collections/{slug}/delete", s.auth.RequireSession(s.handleDeleteCollection))
	mux.HandleFunc("GET /entries", s.auth.RequireSession(s.handleEntries))
	mux.HandleFunc("GET /entries/new", s.auth.RequireSession(s.handleNewEntry))
	mux.HandleFunc("POST /entries", s.auth.RequireSession(s.handleCreateEntry))
	mux.HandleFunc("GET /entries/{slug}/edit", s.auth.RequireSession(s.handleEditEntry))
	mux.HandleFunc("POST /entries/{slug}", s.auth.RequireSession(s.handleUpdateEntry))
	mux.HandleFunc("POST /entries/{slug}/delete", s.auth.RequireSession(s.handleDeleteEntry))
	mux.HandleFunc("GET /series", s.auth.RequireSession(s.handleSeriesList))
	mux.HandleFunc("GET /series/new", s.auth.RequireSession(s.handleNewSeries))
	mux.HandleFunc("POST /series", s.auth.RequireSession(s.handleCreateSeries))
	mux.HandleFunc("GET /series/{slug}/edit", s.auth.RequireSession(s.handleEditSeries))
	mux.HandleFunc("POST /series/{slug}", s.auth.RequireSession(s.handleUpdateSeries))
	mux.HandleFunc("POST /series/{slug}/delete", s.auth.RequireSession(s.handleDeleteSeries))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// Public JSON read API — published content only.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/collections", s.handleAPICollections)
	mux.HandleFunc("GET /api/entries", s.handleAPIEntries)
	mux.HandleFunc("GET /api/entries/{slug}", s.handleAPIEntry)
	mux.HandleFunc("GET /api/series", s.handleAPISeries)
	mux.HandleFunc("GET /api/series/{slug}", s.handleAPISeriesOne)

	// JSON write API — API-key-gated.
	mux.HandleFunc("POST /api/entries", s.auth.RequireAPIKey(s.handleAPICreateEntry))
	mux.HandleFunc("PUT /api/entries/{slug}", s.auth.RequireAPIKey(s.handleAPIUpdateEntry))
	mux.HandleFunc("DELETE /api/entries/{slug}", s.auth.RequireAPIKey(s.handleAPIDeleteEntry))
	mux.HandleFunc("POST /api/series", s.auth.RequireAPIKey(s.handleAPICreateSeries))

	// Editor embedding — session-gated proxy so service keys stay server-side.
	mux.HandleFunc("POST /embed/blob", s.auth.RequireSession(s.handleEmbedBlob))
	mux.HandleFunc("POST /embed/series", s.auth.RequireSession(s.handleEmbedSeries))

	// Shared theme stylesheet and editor script.
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())
	mux.HandleFunc("GET /static/editor.js", theme.EditorJSHandler())

	// Everything content serves is text — HTML, JSON — so Gzip wraps the
	// whole mux. Logging sits outside so the recorded status is the final one;
	// pulse traffic recording sits innermost so logged timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(pulse.Middleware(s.db, "content")(mux))),
		"GET", "POST", "PUT", "DELETE", "OPTIONS")
}

// ── HTML admin: dashboard ──────────────────────────────────────────────────

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	recent, err := listEntries(s.db, "", false, 12)
	if err != nil {
		s.fail(w, "list entries", err)
		return
	}
	total, err := countEntries(s.db, "", false)
	if err != nil {
		s.fail(w, "count entries", err)
		return
	}
	s.rd.Render(w, "dashboard.html", map[string]any{
		"Collections": collections,
		"Entries":     recent,
		"TotalCount":  total,
	})
}

// ── HTML admin: collections ────────────────────────────────────────────────

func (s *Server) handleNewCollection(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "collection_form.html", map[string]any{
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
	entries, err := listEntries(s.db, filter, false, 0)
	if err != nil {
		s.fail(w, "list entries", err)
		return
	}
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	s.rd.Render(w, "entries.html", map[string]any{
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
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

// ── public JSON read API ───────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countCollections(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "content", "ok": true, "collections": n,
	})
}

func (s *Server) handleAPICollections(w http.ResponseWriter, r *http.Request) {
	collections, err := listCollections(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list collections")
		return
	}
	if collections == nil {
		collections = []Collection{}
	}
	// Collections lack an updated_at column, so there is no cheap pre-query
	// fingerprint that catches renames — the ETag comes from the loaded rows
	// instead. The rows are tiny; the 304 saves serialization and bandwidth.
	web.WriteRecord(w, r, cid.OfValue(collections), map[string]any{"collections": collections})
}

func (s *Server) handleAPIEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	collection := q.Get("collection")
	limit, page := parsePaging(q.Get("limit"), q.Get("page"))

	// List-level ETag from a cheap fingerprint, checked before the full list
	// query — an unchanged client revalidates without a single body loading.
	fp, err := entriesFingerprint(s.db, collection)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list entries")
		return
	}
	if listETagDone(w, r, fmt.Sprintf("entries|%s|%d|%d|%s", collection, limit, page, fp)) {
		return
	}

	offset := 0
	if limit > 0 {
		offset = (page - 1) * limit
	}
	entries, err := listEntriesFull(s.db, collection, true, limit, offset)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list entries")
		return
	}
	if entries == nil {
		entries = []Entry{}
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// maxPageSize caps a requested page of /api/entries.
const maxPageSize = 500

// parsePaging reads ?limit= and ?page= (1-based). No params means the full
// list (limit 0) — the original, backward-compatible response. An explicit
// limit is capped at maxPageSize; a page without a limit implies the cap.
func parsePaging(limitStr, pageStr string) (limit, page int) {
	if limitStr != "" {
		limit, _ = strconv.Atoi(limitStr)
	}
	page = 1
	if p, err := strconv.Atoi(pageStr); err == nil && p > 1 {
		page = p
	}
	if limit <= 0 {
		limit = 0
		if page > 1 {
			limit = maxPageSize
		}
	} else if limit > maxPageSize {
		limit = maxPageSize
	}
	return limit, page
}

// listETagDone hashes fingerprint into a list-level ETag, sets the caching
// headers, and reports whether the request was satisfied with a 304.
func listETagDone(w http.ResponseWriter, r *http.Request, fingerprint string) bool {
	etag := cid.Of([]byte(fingerprint))
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	if web.ETagMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

func (s *Server) handleAPIEntry(w http.ResponseWriter, r *http.Request) {
	e, err := getEntry(s.db, r.PathValue("slug"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read entry")
		return
	}
	if e == nil || !e.Published {
		web.WriteError(w, http.StatusNotFound, "entry not found")
		return
	}
	web.WriteRecord(w, r, e.CID, e)
}

// ── API-key-gated write API ────────────────────────────────────────────────

func (s *Server) handleAPICreateEntry(w http.ResponseWriter, r *http.Request) {
	var e Entry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if e.Slug == "" {
		e.Slug = slugify(e.Title)
	}
	if e.Title == "" || e.Slug == "" || e.Collection == "" {
		web.WriteError(w, http.StatusBadRequest, "title, slug, and collection are required")
		return
	}
	if err := insertEntry(s.db, &e); err != nil {
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	web.WriteJSON(w, http.StatusCreated, e)
}

func (s *Server) handleAPIUpdateEntry(w http.ResponseWriter, r *http.Request) {
	current := r.PathValue("slug")
	var e Entry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if e.Slug == "" {
		e.Slug = current
	}
	if err := updateEntry(s.db, current, &e); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			web.WriteError(w, http.StatusNotFound, "entry not found")
			return
		}
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	web.WriteJSON(w, http.StatusOK, e)
}

func (s *Server) handleAPIDeleteEntry(w http.ResponseWriter, r *http.Request) {
	existed, err := deleteEntry(s.db, r.PathValue("slug"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete entry")
		return
	}
	if !existed {
		web.WriteError(w, http.StatusNotFound, "entry not found")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("slug")})
}

// ── rendering helpers ──────────────────────────────────────────────────────

func (s *Server) renderCollectionForm(w http.ResponseWriter, c *Collection, isNew bool, action, errMsg string) {
	s.rd.Render(w, "collection_form.html", map[string]any{
		"Collection": c, "IsNew": isNew, "Action": action, "Error": errMsg,
	})
}

func (s *Server) renderEntryForm(w http.ResponseWriter, e *Entry, collections []Collection, isNew bool, action, errMsg string) {
	s.rd.Render(w, "entry_form.html", map[string]any{
		"Entry": e, "Collections": collections, "IsNew": isNew,
		"Action": action, "Error": errMsg, "TagsText": strings.Join(e.Tags, ", "),
		"BlobsPublic": s.blobsPublic, "ContentPublic": s.contentPublic,
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

// fail logs an internal error and returns a 500.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
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
	s.rd.Render(w, "series.html", map[string]any{"Series": series})
}

func (s *Server) handleNewSeries(w http.ResponseWriter, r *http.Request) {
	s.renderSeriesForm(w, &Series{}, true, "/series", "")
}

func (s *Server) handleCreateSeries(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	title := strings.TrimSpace(r.FormValue("title"))
	se := &Series{
		Slug:  slugify(firstNonEmpty(r.FormValue("slug"), title)),
		Title: title,
		Body:  r.FormValue("body"),
	}
	if se.Slug == "" {
		s.renderSeriesForm(w, se, true, "/series", "A series needs a slug or a title.")
		return
	}
	if existing, _ := getSeries(s.db, se.Slug); existing != nil {
		s.renderSeriesForm(w, se, true, "/series", "That slug is already taken.")
		return
	}
	now := store.NowRFC3339()
	se.CreatedAt, se.UpdatedAt = now, now
	if err := upsertSeries(s.db, se); err != nil {
		s.fail(w, "create series", err)
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

func (s *Server) handleEditSeries(w http.ResponseWriter, r *http.Request) {
	se, err := getSeries(s.db, r.PathValue("slug"))
	if err != nil {
		s.fail(w, "get series", err)
		return
	}
	if se == nil {
		http.NotFound(w, r)
		return
	}
	s.renderSeriesForm(w, se, false, "/series/"+se.Slug, "")
}

func (s *Server) handleUpdateSeries(w http.ResponseWriter, r *http.Request) {
	se, err := getSeries(s.db, r.PathValue("slug"))
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
	se.UpdatedAt = store.NowRFC3339()
	if err := upsertSeries(s.db, se); err != nil {
		s.fail(w, "update series", err)
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

func (s *Server) handleDeleteSeries(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteSeries(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "delete series", err)
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

func (s *Server) renderSeriesForm(w http.ResponseWriter, se *Series, isNew bool, action, errMsg string) {
	s.rd.Render(w, "series_form.html", map[string]any{
		"Series": se, "IsNew": isNew, "Action": action, "Error": errMsg,
	})
}

// ── series: public JSON ────────────────────────────────────────────────────

func (s *Server) handleAPISeries(w http.ResponseWriter, r *http.Request) {
	fp, err := seriesFingerprint(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list series")
		return
	}
	if listETagDone(w, r, "series|"+fp) {
		return
	}
	series, err := listSeries(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list series")
		return
	}
	if series == nil {
		series = []Series{}
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"series": series})
}

func (s *Server) handleAPISeriesOne(w http.ResponseWriter, r *http.Request) {
	se, err := getSeries(s.db, r.PathValue("slug"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read series")
		return
	}
	if se == nil {
		web.WriteError(w, http.StatusNotFound, "series not found")
		return
	}
	web.WriteRecord(w, r, se.CID, se)
}
