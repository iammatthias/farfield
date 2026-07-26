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
	"time"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/keys"
	"github.com/iammatthias/farfield/lib/markdown"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates static
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

	// fleet search sources — internal URLs + optional read keys
	feedURL          string
	feedReadKey      string
	bookmarksURL     string
	bookmarksReadKey string
	siteURLTmpl      string // public page URL pattern with {collection}/{slug} holes; "" = no view-on-site link

	// md renders markdown bodies for the admin UI — the document preview on
	// the edit page and the editor's live preview endpoint.
	md *markdown.Renderer

	// rl rate-limits the public, ungated single-entry read (the "view source"
	// endpoint) per client IP. Keyed callers are exempt; drafts stay 404 to
	// anonymous callers (only the write key previews them).
	rl *web.RateLimiter

	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder
}

// publicReadPerMin caps anonymous hits to the public single-entry read endpoint
// per client IP per minute. Keyed callers (e.g. the site's server-side fetches)
// bypass it, so this only throttles unauthenticated "view source" traffic.
const publicReadPerMin = 60

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
			ReadKey:      store.Env("CONTENT_READ_KEY", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd: &web.Renderer{Templates: tmpl, AssetVer: theme.Version,
			App: "content", Mark: "co",
			Nav: []web.NavItem{
				{Label: "Dashboard", URL: "/"},
				{Label: "Entries", URL: "/entries"},
				{Label: "Series", URL: "/series"},
				{Label: "Log out", URL: "/logout"},
			},
		},
		blobsURL:      store.Env("BLOBS_URL", "http://127.0.0.1:8789"),
		blobsKey:      store.Env("BLOBS_API_KEY", ""),
		blobsPublic:   store.Env("BLOBS_PUBLIC_URL", "http://127.0.0.1:8789"),
		contentPublic: store.Env("CONTENT_PUBLIC_URL", "http://127.0.0.1:8787"),
		siteURLTmpl:   store.Env("SITE_URL_TEMPLATE", ""), // e.g. https://example.com/{collection}/{slug}

		feedURL:          store.Env("FEED_URL", "http://127.0.0.1:8788"),
		feedReadKey:      store.Env("FEED_READ_KEY", ""),
		bookmarksURL:     store.Env("BOOKMARKS_URL", "http://127.0.0.1:8793"),
		bookmarksReadKey: store.Env("BOOKMARKS_READ_KEY", ""),
	}

	s.md = newRenderer(s.db, s.blobsURL, s.blobsPublic)

	defer keys.Attach(s.auth, "content")() // admin-issued keys, when KEYS_DB_PATH is set

	s.pulse = pulse.New(s.db, "content")
	defer s.pulse.Close()
	return web.Serve(host, port, s.routes())
}

func (s *Server) routes() http.Handler {
	if s.rl == nil {
		s.rl = web.NewRateLimiter(publicReadPerMin, time.Minute)
	}
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
	mux.HandleFunc("GET /entries/trash", s.auth.RequireSession(s.handleTrash))
	mux.HandleFunc("GET /search", s.auth.RequireSession(s.handleFleetSearchPage))
	mux.HandleFunc("GET /fleet-search-data", s.auth.RequireSession(s.handleFleetSearchData))
	mux.HandleFunc("POST /entries", s.auth.RequireSession(s.handleCreateEntry))
	mux.HandleFunc("GET /entries/{slug}/edit", s.auth.RequireSession(s.handleEditEntry))
	mux.HandleFunc("POST /entries/{slug}", s.auth.RequireSession(s.handleUpdateEntry))
	mux.HandleFunc("POST /entries/{slug}/delete", s.auth.RequireSession(s.handleDeleteEntry))
	mux.HandleFunc("POST /entries/{slug}/restore", s.auth.RequireSession(s.handleRestoreEntry))
	mux.HandleFunc("POST /entries/{slug}/destroy", s.auth.RequireSession(s.handleDestroyEntry))
	mux.HandleFunc("POST /entries/{slug}/revisions/{id}/restore", s.auth.RequireSession(s.handleRestoreRevision))
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

	// JSON read API — bearer-token-gated when CONTENT_READ_KEY is set (the
	// write CONTENT_API_KEY is also accepted, and unlocks drafts for preview).
	// /status stays public so the healthcheck and uptime probes never need a
	// token. Published content only, unless the request carries the write key.
	//
	// A single PUBLISHED entry by slug is the exception: it is public (so the
	// site's "view source" links open in a browser) but rate-limited per client
	// IP, with keyed callers exempt. Draft protection is unchanged — handleAPIEntry
	// 404s a draft unless the write key is present, so anonymous callers can never
	// see one. The enumerating lists, collections, and series stay token-gated
	// (a series can back an unpublished entry).
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/collections", s.auth.RequireReadKey(s.handleAPICollections))
	mux.HandleFunc("GET /api/entries", s.auth.RequireReadKey(s.handleAPIEntries))
	mux.HandleFunc("GET /api/entries/{slug}", web.RateLimit(s.rl, s.auth.HasReadKey, s.handleAPIEntry))
	mux.HandleFunc("GET /api/series", s.auth.RequireReadKey(s.handleAPISeries))
	mux.HandleFunc("GET /api/series/{slug}", s.auth.RequireReadKey(s.handleAPISeriesOne))

	// JSON write API — API-key-gated.
	mux.HandleFunc("POST /api/entries", s.auth.RequireAPIKey(s.handleAPICreateEntry))
	mux.HandleFunc("PUT /api/entries/{slug}", s.auth.RequireAPIKey(s.handleAPIUpdateEntry))
	mux.HandleFunc("DELETE /api/entries/{slug}", s.auth.RequireAPIKey(s.handleAPIDeleteEntry))
	mux.HandleFunc("POST /api/series", s.auth.RequireAPIKey(s.handleAPICreateSeries))

	// Editor embedding — session-gated proxy so service keys stay server-side.
	// The list reads (blob gallery, series picker) proxy the now-token-gated
	// sibling APIs so the editor page never needs a read token.
	mux.HandleFunc("POST /preview", s.auth.RequireSession(s.handlePreview))
	mux.HandleFunc("POST /editdoc", s.auth.RequireSession(s.handleEditdoc))
	mux.HandleFunc("POST /embed/blob", s.auth.RequireSession(s.handleEmbedBlob))
	mux.HandleFunc("POST /embed/series", s.auth.RequireSession(s.handleEmbedSeries))
	mux.HandleFunc("GET /embed/blobs", s.auth.RequireSession(s.handleEmbedBlobsList))
	mux.HandleFunc("GET /embed/series", s.auth.RequireSession(s.handleEmbedSeriesList))

	// Shared theme stylesheet and editor script.
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())
	mux.HandleFunc("GET /static/editor.js", theme.EditorJSHandler())

	// App-local static assets. The vendored semantic-search engine is
	// versioned by URL (?v=) and immutable; everything else revalidates.
	// The exact patterns above outrank this subtree.
	mux.Handle("GET /static/", staticHandler())

	// Search corpus for the entries page — session-gated like the page.
	mux.HandleFunc("GET /search-data", s.auth.RequireSession(s.handleSearchData))

	// Everything content serves is text — HTML, JSON — so Gzip wraps the
	// whole mux. Logging sits outside so the recorded status is the final one;
	// pulse traffic recording sits innermost so logged timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(s.pulse.Wrap(mux))),
		"GET", "POST", "PUT", "DELETE", "OPTIONS")
}

// ── HTML admin: dashboard ──────────────────────────────────────────────────

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	recent, err := listEntries(s.db, "", statusAll, 12)
	if err != nil {
		s.fail(w, "list entries", err)
		return
	}
	total, err := countEntries(s.db, "", statusAll)
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
	entries, err := listEntries(s.db, filter, statusAll, 0)
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
	s.renderEntryForm(w, r, &Entry{Published: false}, collections, true, "/entries", "")
}

func (s *Server) handleCreateEntry(w http.ResponseWriter, r *http.Request) {
	e := entryFromForm(r)
	if e.Title == "" || e.Slug == "" || e.Collection == "" {
		s.entrySaveError(w, r, e, true, "/entries", "Title and collection are required.")
		return
	}
	if err := insertEntry(s.db, e); err != nil {
		s.entrySaveError(w, r, e, true, "/entries", err.Error())
		return
	}
	s.entrySaved(w, r, e)
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
	s.renderEntryForm(w, r, e, collections, false, "/entries/"+e.Slug, "")
}

// handleRestoreRevision copies a saved revision's title and body back onto
// the entry. The restore is itself a normal save, so it lands in history too
// — restoring can never destroy state.
func (s *Server) handleRestoreRevision(w http.ResponseWriter, r *http.Request) {
	e, err := getEntry(s.db, r.PathValue("slug"))
	if err != nil {
		s.fail(w, "get entry", err)
		return
	}
	revID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rev, err := getRevision(s.db, revID)
	if err != nil {
		s.fail(w, "get revision", err)
		return
	}
	if e == nil || rev == nil || rev.EntryID != e.ID {
		http.NotFound(w, r)
		return
	}
	e.Title, e.Body = rev.Title, rev.Body
	if err := updateEntry(s.db, e.Slug, e); err != nil {
		s.fail(w, "restore revision", err)
		return
	}
	http.Redirect(w, r, "/entries/"+e.Slug+"/edit", http.StatusSeeOther)
}

func (s *Server) handleUpdateEntry(w http.ResponseWriter, r *http.Request) {
	current := r.PathValue("slug")
	e := entryFromForm(r)
	if e.Title == "" || e.Slug == "" || e.Collection == "" {
		s.entrySaveError(w, r, e, false, "/entries/"+current, "Title and collection are required.")
		return
	}
	if err := updateEntry(s.db, current, e); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		s.entrySaveError(w, r, e, false, "/entries/"+current, err.Error())
		return
	}
	s.entrySaved(w, r, e)
}

// entrySaved answers a successful create or update: JSON with the entry's
// canonical URLs for the editor's async saves (the slug may have changed),
// a redirect to the list for a plain form post.
func (s *Server) entrySaved(w http.ResponseWriter, r *http.Request, e *Entry) {
	if wantsJSON(r) {
		web.WriteJSON(w, http.StatusOK, map[string]any{
			"slug":    e.Slug,
			"action":  "/entries/" + e.Slug,
			"editURL": "/entries/" + e.Slug + "/edit",
		})
		return
	}
	http.Redirect(w, r, "/entries", http.StatusSeeOther)
}

// entrySaveError answers a failed create or update: a JSON error for the
// editor's async saves, the re-rendered form for a plain post.
func (s *Server) entrySaveError(w http.ResponseWriter, r *http.Request, e *Entry, isNew bool, action, msg string) {
	if wantsJSON(r) {
		web.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	s.reRenderEntryForm(w, r, e, isNew, action, msg)
}

// handleDeleteEntry moves an entry to the trash. The delete is soft — with
// revisions covering edits and the trash covering deletes, no entry admin
// action is destructive anymore; "delete forever" lives on the trash page.
func (s *Server) handleDeleteEntry(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteEntry(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "delete entry", err)
		return
	}
	http.Redirect(w, r, "/entries", http.StatusSeeOther)
}

// ── HTML admin: trash ──────────────────────────────────────────────────────

// handleTrash lists soft-deleted entries with per-row restore and
// delete-forever actions. Trashed rows older than trashRetention are purged
// at startup.
func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request) {
	trashed, err := listTrash(s.db)
	if err != nil {
		s.fail(w, "list trash", err)
		return
	}
	s.rd.Render(w, "trash.html", map[string]any{
		"Entries": trashed, "RetentionDays": int(trashRetention.Hours() / 24),
	})
}

// handleRestoreEntry returns a trashed entry to the live lists.
func (s *Server) handleRestoreEntry(w http.ResponseWriter, r *http.Request) {
	if _, err := restoreEntry(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "restore entry", err)
		return
	}
	http.Redirect(w, r, "/entries/trash", http.StatusSeeOther)
}

// handleDestroyEntry hard-deletes a trashed entry — the one genuinely
// destructive entry action left, and it only works on rows already in the
// trash (a live entry must be trashed first).
func (s *Server) handleDestroyEntry(w http.ResponseWriter, r *http.Request) {
	if _, err := hardDeleteEntry(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "destroy entry", err)
		return
	}
	http.Redirect(w, r, "/entries/trash", http.StatusSeeOther)
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
	status, ok := s.resolveStatus(w, r, q.Get("status"))
	if !ok {
		return
	}
	limit, page := parsePaging(q.Get("limit"), q.Get("page"))

	// List-level ETag from a cheap fingerprint, checked before the full list
	// query — an unchanged client revalidates without a single body loading.
	// The status is in the fingerprint so a draft view never reuses a published
	// list's ETag.
	fp, err := entriesFingerprint(s.db, collection, status)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list entries")
		return
	}
	if listETagDone(w, r, fmt.Sprintf("entries|%s|%d|%d|%d|%s", collection, int(status), limit, page, fp)) {
		return
	}

	offset := 0
	if limit > 0 {
		offset = (page - 1) * limit
	}
	entries, err := listEntriesFull(s.db, collection, status, limit, offset)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list entries")
		return
	}
	if entries == nil {
		entries = []Entry{}
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// resolveStatus maps the ?status= query to an entryStatus and enforces that
// draft visibility (status=draft or status=all) requires the write key — the
// read token alone only ever sees published content. It writes the error
// response and returns ok=false when the request is rejected.
func (s *Server) resolveStatus(w http.ResponseWriter, r *http.Request, param string) (entryStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(param)) {
	case "", "published":
		return statusPublished, true
	case "draft", "drafts":
		if !s.auth.HasWriteKey(r) {
			web.WriteError(w, http.StatusForbidden, "drafts require the write API key")
			return statusPublished, false
		}
		return statusDraft, true
	case "all":
		if !s.auth.HasWriteKey(r) {
			web.WriteError(w, http.StatusForbidden, "drafts require the write API key")
			return statusPublished, false
		}
		return statusAll, true
	default:
		web.WriteError(w, http.StatusBadRequest, "unknown status (use published, draft, or all)")
		return statusPublished, false
	}
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
	// A draft is fetchable by slug only with the write key — that is the
	// preview path. To the read token a draft is indistinguishable from a
	// missing entry.
	if e == nil || (!e.Published && !s.auth.HasWriteKey(r)) {
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

// handleAPIDeleteEntry trashes an entry, like the admin delete — soft, so no
// API action is destructive either. The response shape is unchanged: to the
// caller the entry is deleted (and reads as gone); restore and purge live in
// the admin trash page.
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

func (s *Server) renderEntryForm(w http.ResponseWriter, r *http.Request, e *Entry, collections []Collection, isNew bool, action, errMsg string) {
	s.rd.Render(w, "entry_form.html", map[string]any{
		"Entry": e, "Collections": collections, "IsNew": isNew,
		"Action": action, "Error": errMsg, "TagsText": strings.Join(e.Tags, ", "),
		"BlobsPublic": s.blobsPublic, "ContentPublic": s.contentPublic,
		"BodyHTML": s.bodyHTML(r, e.Body), "Words": wordCount(e.Body),
		"Revisions": s.revisionViews(e), "SiteURL": s.siteURL(e),
	})
}

// siteURL is the entry's public page — SITE_URL_TEMPLATE with {collection}
// and {slug} filled in. Empty when the template is unset (feature off) or the
// entry is unpublished (a draft has no public page yet), so the template just
// checks .SiteURL.
func (s *Server) siteURL(e *Entry) string {
	if s.siteURLTmpl == "" || !e.Published {
		return ""
	}
	return strings.NewReplacer(
		"{collection}", e.Collection,
		"{slug}", e.Slug,
	).Replace(s.siteURLTmpl)
}

// revisionViews shapes an entry's history for the edit page rail.
func (s *Server) revisionViews(e *Entry) []map[string]any {
	if e.ID == 0 {
		return nil
	}
	revs, err := listRevisions(s.db, e.ID, 10)
	if err != nil {
		slog.Warn("list revisions", "slug", e.Slug, "err", err)
		return nil
	}
	views := make([]map[string]any, 0, len(revs))
	for _, rv := range revs {
		when := rv.SavedAt
		if t, err := time.Parse(time.RFC3339, rv.SavedAt); err == nil {
			when = t.Local().Format("Jan 2 15:04")
		}
		views = append(views, map[string]any{
			"ID": rv.ID, "When": when,
			"Words":   revisionWords(rv.Body),
			"Current": rv.CID == e.CID,
		})
	}
	return views
}

// reRenderEntryForm re-shows the entry form after a failed submit.
func (s *Server) reRenderEntryForm(w http.ResponseWriter, r *http.Request, e *Entry, isNew bool, action, errMsg string) {
	collections, err := listCollections(s.db)
	if err != nil {
		s.fail(w, "list collections", err)
		return
	}
	s.renderEntryForm(w, r, e, collections, isNew, action, errMsg)
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
	s.renderSeriesForm(w, r, &Series{}, true, "/series", "")
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
		s.seriesSaveError(w, r, se, true, "/series", "A series needs a slug or a title.")
		return
	}
	if existing, _ := getSeries(s.db, se.Slug); existing != nil {
		s.seriesSaveError(w, r, se, true, "/series", "That slug is already taken.")
		return
	}
	now := store.NowRFC3339()
	se.CreatedAt, se.UpdatedAt = now, now
	if err := upsertSeries(s.db, se); err != nil {
		s.fail(w, "create series", err)
		return
	}
	s.seriesSaved(w, r, se)
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
	s.renderSeriesForm(w, r, se, false, "/series/"+se.Slug, "")
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
	s.seriesSaved(w, r, se)
}

// seriesSaved and seriesSaveError mirror the entry save responses for the
// series form's async saves.
func (s *Server) seriesSaved(w http.ResponseWriter, r *http.Request, se *Series) {
	if wantsJSON(r) {
		web.WriteJSON(w, http.StatusOK, map[string]any{
			"slug":    se.Slug,
			"action":  "/series/" + se.Slug,
			"editURL": "/series/" + se.Slug + "/edit",
		})
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

func (s *Server) seriesSaveError(w http.ResponseWriter, r *http.Request, se *Series, isNew bool, action, msg string) {
	if wantsJSON(r) {
		web.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	s.renderSeriesForm(w, r, se, isNew, action, msg)
}

func (s *Server) handleDeleteSeries(w http.ResponseWriter, r *http.Request) {
	if _, err := deleteSeries(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "delete series", err)
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

func (s *Server) renderSeriesForm(w http.ResponseWriter, r *http.Request, se *Series, isNew bool, action, errMsg string) {
	s.rd.Render(w, "series_form.html", map[string]any{
		"Series": se, "IsNew": isNew, "Action": action, "Error": errMsg,
		"BlobsPublic": s.blobsPublic, "ContentPublic": s.contentPublic,
		"BodyHTML": s.bodyHTML(r, se.Body), "Words": wordCount(se.Body),
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
