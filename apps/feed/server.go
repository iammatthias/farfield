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

// Server holds the running feed service.
type Server struct {
	db            *sql.DB
	templates     map[string]*template.Template
	password      string
	apiKey        string
	cookieSecure  bool
	blobsURL      string // internal blobs service URL — for the upload proxy
	blobsKey      string // blobs API key — kept server-side
	contentURL    string // internal content service URL — for series creation
	contentKey    string // content API key — kept server-side
	blobsPublic   string // browser-facing blobs URL — injected into the editor
	contentPublic string // browser-facing content URL — injected into the editor
	assetVer      string // content hash of the static assets — cache-busts URLs
}

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("FEED_DB_PATH", "feed.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	tmpl, err := parseTemplates()
	if err != nil {
		return err
	}

	s := &Server{
		db:            db,
		templates:     tmpl,
		password:      store.Env("PASSWORD", ""),
		apiKey:        store.Env("FEED_API_KEY", ""),
		cookieSecure:  store.Env("COOKIE_SECURE", "false") == "true",
		blobsURL:      store.Env("BLOBS_URL", "http://127.0.0.1:8789"),
		blobsKey:      store.Env("BLOBS_API_KEY", ""),
		contentURL:    store.Env("CONTENT_URL", "http://127.0.0.1:8787"),
		contentKey:    store.Env("CONTENT_API_KEY", ""),
		blobsPublic:   store.Env("BLOBS_PUBLIC_URL", "http://127.0.0.1:8789"),
		contentPublic: store.Env("CONTENT_PUBLIC_URL", "http://127.0.0.1:8787"),
		assetVer:      cid.Of([]byte(theme.CSS + theme.EditorJS))[:16],
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
	mux.HandleFunc("GET /new", s.requireSession(s.handleNewPost))
	mux.HandleFunc("POST /posts", s.requireSession(s.handleCreatePost))
	mux.HandleFunc("GET /posts/{slug}/edit", s.requireSession(s.handleEditPost))
	mux.HandleFunc("POST /posts/{slug}", s.requireSession(s.handleUpdatePost))
	mux.HandleFunc("POST /posts/{slug}/delete", s.requireSession(s.handleDeletePost))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /logout", s.handleLogout)

	// Public JSON read API.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/posts", s.handleAPIList)
	mux.HandleFunc("GET /api/posts/{slug}", s.handleAPIGet)

	// JSON write API — API-key-gated.
	mux.HandleFunc("POST /api/posts", s.requireAPIKey(s.handleAPICreate))
	mux.HandleFunc("PUT /api/posts/{slug}", s.requireAPIKey(s.handleAPIUpdate))
	mux.HandleFunc("DELETE /api/posts/{slug}", s.requireAPIKey(s.handleAPIDelete))

	// Editor embedding — session-gated proxy so service keys stay server-side.
	mux.HandleFunc("POST /embed/blob", s.requireSession(s.handleEmbedBlob))
	mux.HandleFunc("POST /embed/series", s.requireSession(s.handleEmbedSeries))

	// Shared theme stylesheet and editor script.
	mux.HandleFunc("GET /static/styles.css", handleCSS)
	mux.HandleFunc("GET /static/editor.js", handleEditorJS)

	return cors(logRequests(mux))
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
	posts, err := listPosts(s.db)
	if err != nil {
		s.fail(w, "list posts", err)
		return
	}
	renderer := newBodyRenderer(s.blobsURL, s.blobsPublic)
	views := make([]postView, 0, len(posts))
	for _, p := range posts {
		views = append(views, postView{Post: p, BodyHTML: renderer.render(p.Body)})
	}
	s.render(w, "index.html", map[string]any{"Posts": views})
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
	posts, err := listPosts(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "feed", "ok": true, "posts": len(posts),
	})
}

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	posts, err := listPosts(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list posts")
		return
	}
	if posts == nil {
		posts = []Post{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"posts": posts})
}

func (s *Server) handleAPIGet(w http.ResponseWriter, r *http.Request) {
	p, err := getPost(s.db, r.PathValue("slug"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read post")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "post not found")
		return
	}
	writeRecord(w, r, p.CID, p)
}

// ── API-key-gated write API ────────────────────────────────────────────────

func (s *Server) handleAPICreate(w http.ResponseWriter, r *http.Request) {
	var p Post
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(p.Body) == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}
	p.Slug = "" // server-assigned
	if err := insertPost(s.db, &p); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create post")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleAPIUpdate(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var p Post
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(p.Body) == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}
	ok, err := updatePost(s.db, slug, &p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not update post")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "post not found")
		return
	}
	p.Slug = slug
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	existed, err := deletePost(s.db, r.PathValue("slug"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete post")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "post not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("slug")})
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
	s.render(w, "post_form.html", map[string]any{
		"Post": p, "IsNew": isNew, "Action": action, "Error": errMsg,
		"TagsText":    strings.Join(p.Tags, ", "),
		"BlobsPublic": s.blobsPublic, "ContentPublic": s.contentPublic,
	})
}

func handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(w, theme.CSS)
}

func handleEditorJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(w, theme.EditorJS)
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
	// Stamp the static-asset version into every page so cache-busted URLs
	// (styles.css, editor.js) update the moment a new build ships.
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
func writeRecord(w http.ResponseWriter, r *http.Request, cid string, v any) {
	etag := `"` + cid + `"`
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
