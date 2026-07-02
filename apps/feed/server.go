package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/keys"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

// Server holds the running feed service.
type Server struct {
	db            *sql.DB
	auth          *web.Auth
	rd            *web.Renderer
	blobsURL      string // internal blobs service URL — for the upload proxy
	blobsKey      string // blobs API key — kept server-side
	contentURL    string // internal content service URL — for series creation
	contentKey    string // content API key — kept server-side
	blobsPublic   string // browser-facing blobs URL — injected into the editor
	contentPublic string // browser-facing content URL — injected into the editor

	// rl rate-limits the public, ungated single-post read (the "view source"
	// endpoint) per client IP. Keyed callers are exempt.
	rl *web.RateLimiter

	// blobCache memoizes successful blob metadata lookups for the lifetime
	// of the server. Blob CIDs are content-addressed and immutable, so a
	// cached entry never goes stale.
	blobCache sync.Map // cid → blobLookup

	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder
}

// pageSize is how many posts one admin index page or default API list holds.
const pageSize = 50

// apiMaxLimit caps the ?limit= an API client may request.
const apiMaxLimit = 200

// publicReadPerMin caps anonymous hits to the public single-post read endpoint
// per client IP per minute. Keyed callers (e.g. the site's server-side fetches)
// bypass it, so this only throttles unauthenticated "view source" traffic.
const publicReadPerMin = 60

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("FEED_DB_PATH", "feed.sqlite"))
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
			APIKey:       store.Env("FEED_API_KEY", ""),
			ReadKey:      store.Env("FEED_READ_KEY", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd:            &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
		blobsURL:      store.Env("BLOBS_URL", "http://127.0.0.1:8789"),
		blobsKey:      store.Env("BLOBS_API_KEY", ""),
		contentURL:    store.Env("CONTENT_URL", "http://127.0.0.1:8787"),
		contentKey:    store.Env("CONTENT_API_KEY", ""),
		blobsPublic:   store.Env("BLOBS_PUBLIC_URL", "http://127.0.0.1:8789"),
		contentPublic: store.Env("CONTENT_PUBLIC_URL", "http://127.0.0.1:8787"),
	}

	defer keys.Attach(s.auth, "feed")() // admin-issued keys, when KEYS_DB_PATH is set

	s.pulse = pulse.New(s.db, "feed")
	defer s.pulse.Close()
	return web.Serve(host, port, s.routes())
}

func (s *Server) routes() http.Handler {
	if s.rl == nil {
		s.rl = web.NewRateLimiter(publicReadPerMin, time.Minute)
	}
	mux := http.NewServeMux()

	// HTML admin UI — session-gated.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleIndex))
	mux.HandleFunc("GET /new", s.auth.RequireSession(s.handleNewPost))
	mux.HandleFunc("POST /posts", s.auth.RequireSession(s.handleCreatePost))
	mux.HandleFunc("GET /posts/{slug}/edit", s.auth.RequireSession(s.handleEditPost))
	mux.HandleFunc("POST /posts/{slug}", s.auth.RequireSession(s.handleUpdatePost))
	mux.HandleFunc("POST /posts/{slug}/delete", s.auth.RequireSession(s.handleDeletePost))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// JSON read API. The LIST stays bearer-token-gated when FEED_READ_KEY is set
	// (it enumerates every post; the write FEED_API_KEY is also accepted). A
	// single post by slug is PUBLIC so the site's "view source" links load in a
	// browser — the slug must already be known and the body is published anyway —
	// but rate-limited per client IP. Keyed callers (the site's server-side
	// fetches) are exempt. /status stays public for the healthcheck.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/posts", s.auth.RequireReadKey(s.handleAPIList))
	mux.HandleFunc("GET /api/posts/{slug}", web.RateLimit(s.rl, s.auth.HasReadKey, s.handleAPIGet))

	// JSON write API — API-key-gated.
	mux.HandleFunc("POST /api/posts", s.auth.RequireAPIKey(s.handleAPICreate))
	mux.HandleFunc("PUT /api/posts/{slug}", s.auth.RequireAPIKey(s.handleAPIUpdate))
	mux.HandleFunc("DELETE /api/posts/{slug}", s.auth.RequireAPIKey(s.handleAPIDelete))

	// Editor embedding — session-gated proxy so service keys stay server-side.
	// The list reads (blob gallery, series picker) proxy the now-token-gated
	// sibling APIs so the editor page never needs a read token.
	mux.HandleFunc("POST /embed/blob", s.auth.RequireSession(s.handleEmbedBlob))
	mux.HandleFunc("POST /embed/series", s.auth.RequireSession(s.handleEmbedSeries))
	mux.HandleFunc("GET /embed/blobs", s.auth.RequireSession(s.handleEmbedBlobsList))
	mux.HandleFunc("GET /embed/series", s.auth.RequireSession(s.handleEmbedSeriesList))

	// Shared theme stylesheet and editor script.
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())
	mux.HandleFunc("GET /static/editor.js", theme.EditorJSHandler())

	// Everything feed serves is text — HTML, JSON; media bytes live in the
	// blobs service — so Gzip wraps the whole mux. Logging sits outside so
	// the recorded status is the final one; pulse traffic recording sits
	// innermost so logged timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(s.pulse.Wrap(mux))),
		"GET", "POST", "PUT", "DELETE", "OPTIONS")
}

// postFromForm reads a Post from a posted admin form. A form parse failure
// surfaces as an error so callers answer 400 instead of treating the request
// as an empty post.
func postFromForm(r *http.Request) (*Post, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return &Post{
		Body: strings.TrimSpace(r.FormValue("body")),
		Tags: splitTags(r.FormValue("tags")),
	}, nil
}

// ── HTML admin handlers ────────────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	before := r.URL.Query().Get("before")
	// Fetch one past the page to learn whether an older page exists.
	posts, err := listPosts(s.db, pageSize+1, before)
	if err != nil {
		s.fail(w, "list posts", err)
		return
	}
	older := ""
	if len(posts) > pageSize {
		posts = posts[:pageSize]
		older = posts[len(posts)-1].CreatedAt
	}
	renderer := newBodyRenderer(r.Context(), s.blobsURL, s.blobsPublic, &s.blobCache)
	views := make([]postView, 0, len(posts))
	for _, p := range posts {
		views = append(views, postView{Post: p, BodyHTML: renderer.render(p.Body)})
	}
	s.rd.Render(w, "index.html", map[string]any{"Posts": views, "Older": older})
}

func (s *Server) handleNewPost(w http.ResponseWriter, r *http.Request) {
	s.renderPostForm(w, &Post{}, true, "/posts", "")
}

func (s *Server) handleCreatePost(w http.ResponseWriter, r *http.Request) {
	p, err := postFromForm(r)
	if err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if p.Body == "" {
		s.renderPostForm(w, p, true, "/posts", "A post needs a body.")
		return
	}
	if err := insertPost(s.db, p); err != nil {
		s.fail(w, "create post", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleEditPost(w http.ResponseWriter, r *http.Request) {
	p, err := getPost(s.db, r.PathValue("slug"))
	if err != nil {
		s.fail(w, "get post", err)
		return
	}
	if p == nil {
		http.NotFound(w, r)
		return
	}
	s.renderPostForm(w, p, false, "/posts/"+p.Slug, "")
}

func (s *Server) handleUpdatePost(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	p, err := postFromForm(r)
	if err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if p.Body == "" {
		p.Slug = slug
		s.renderPostForm(w, p, false, "/posts/"+slug, "A post needs a body.")
		return
	}
	ok, err := updatePost(s.db, slug, p)
	if err != nil {
		s.fail(w, "update post", err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDeletePost(w http.ResponseWriter, r *http.Request) {
	if _, err := deletePost(s.db, r.PathValue("slug")); err != nil {
		s.fail(w, "delete post", err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ── login ──────────────────────────────────────────────────────────────────

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

// ── public JSON read API ───────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countPosts(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "feed", "ok": true, "posts": n,
	})
}

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	limit := pageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = min(n, apiMaxLimit)
		}
	}
	before := r.URL.Query().Get("before")

	// One cheap aggregate stamps the whole list; clients holding the current
	// version get a 304 before the list query runs. The page parameters are
	// part of the tag — different pages are different representations.
	stamp, err := listStamp(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list posts")
		return
	}
	etag := cid.Of([]byte(stamp + "|" + strconv.Itoa(limit) + "|" + before))
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	if web.ETagMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	posts, err := listPosts(s.db, limit, before)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list posts")
		return
	}
	if posts == nil {
		posts = []Post{}
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"posts": posts})
}

func (s *Server) handleAPIGet(w http.ResponseWriter, r *http.Request) {
	p, err := getPost(s.db, r.PathValue("slug"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read post")
		return
	}
	if p == nil {
		web.WriteError(w, http.StatusNotFound, "post not found")
		return
	}
	web.WriteRecord(w, r, p.CID, p)
}

// ── API-key-gated write API ────────────────────────────────────────────────

func (s *Server) handleAPICreate(w http.ResponseWriter, r *http.Request) {
	var p Post
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(p.Body) == "" {
		web.WriteError(w, http.StatusBadRequest, "body is required")
		return
	}
	p.Slug = "" // server-assigned
	if err := insertPost(s.db, &p); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not create post")
		return
	}
	web.WriteJSON(w, http.StatusCreated, p)
}

func (s *Server) handleAPIUpdate(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var p Post
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(p.Body) == "" {
		web.WriteError(w, http.StatusBadRequest, "body is required")
		return
	}
	ok, err := updatePost(s.db, slug, &p)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not update post")
		return
	}
	if !ok {
		web.WriteError(w, http.StatusNotFound, "post not found")
		return
	}
	p.Slug = slug
	web.WriteJSON(w, http.StatusOK, p)
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	existed, err := deletePost(s.db, r.PathValue("slug"))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete post")
		return
	}
	if !existed {
		web.WriteError(w, http.StatusNotFound, "post not found")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("slug")})
}

// ── helpers ────────────────────────────────────────────────────────────────

// splitTags parses a comma-separated tag input into a trimmed, de-duplicated
// slice. Empty input yields an empty slice.
func splitTags(s string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func (s *Server) renderPostForm(w http.ResponseWriter, p *Post, isNew bool, action, errMsg string) {
	s.rd.Render(w, "post_form.html", map[string]any{
		"Post": p, "IsNew": isNew, "Action": action, "Error": errMsg,
		"TagsText":    strings.Join(p.Tags, ", "),
		"BlobsPublic": s.blobsPublic, "ContentPublic": s.contentPublic,
	})
}

// fail logs an internal error and returns a 500.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
