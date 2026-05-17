package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
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
	"strconv"
	"syscall"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
)

//go:embed templates
var assets embed.FS

const (
	defaultMaxUpload = 100 << 20 // 100 MiB
	pageSize         = 48        // blobs per admin page
)

// Server holds the running blob service.
type Server struct {
	db           *sql.DB
	store        ByteStore
	templates    map[string]*template.Template
	password     string
	apiKey       string
	cookieSecure bool
	maxUpload    int64
}

// openStore selects the byte-store backend from the environment.
func openStore() (ByteStore, string, error) {
	switch store.Env("BLOBS_BACKEND", "local") {
	case "local":
		dir := store.Env("BLOBS_DIR", "blobs-data")
		bs, err := OpenLocalDir(dir)
		return bs, "local:" + dir, err
	case "r2":
		bucket := os.Getenv("R2_BUCKET")
		bs, err := NewR2(R2Config{
			AccountID:       os.Getenv("R2_ACCOUNT_ID"),
			AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
			Bucket:          bucket,
		})
		return bs, "r2:" + bucket, err
	default:
		return nil, "", fmt.Errorf(`BLOBS_BACKEND must be "local" or "r2"`)
	}
}

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("BLOBS_DB_PATH", "blobs.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	bs, desc, err := openStore()
	if err != nil {
		return err
	}
	slog.Info("blob store", "backend", desc)

	tmpl, err := parseTemplates()
	if err != nil {
		return err
	}

	s := &Server{
		db:           db,
		store:        bs,
		templates:    tmpl,
		password:     store.Env("PASSWORD", ""),
		apiKey:       store.Env("BLOBS_API_KEY", ""),
		cookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		maxUpload:    defaultMaxUpload,
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
	mux.HandleFunc("GET /upload", s.requireSession(s.handleUploadForm))
	mux.HandleFunc("POST /upload", s.requireSession(s.handleAdminUpload))
	mux.HandleFunc("POST /blobs/{cid}/delete", s.requireSession(s.handleAdminDelete))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /logout", s.handleLogout)

	// Public JSON / bytes API.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /blobs", s.handleAPIList)
	mux.HandleFunc("GET /blobs/{cid}", s.handleAPIGetBytes)
	mux.HandleFunc("GET /blobs/{cid}/meta", s.handleAPIGetMeta)

	// JSON write API — API-key-gated.
	mux.HandleFunc("POST /blobs", s.requireAPIKey(s.handleAPIUpload))
	mux.HandleFunc("DELETE /blobs/{cid}", s.requireAPIKey(s.handleAPIDelete))

	// Backup storage — API-key-gated opaque snapshots, kept out of the media index.
	mux.HandleFunc("POST /backups", s.requireAPIKey(s.handleBackupPut))
	mux.HandleFunc("GET /backups/{cid}", s.requireAPIKey(s.handleBackupGet))
	mux.HandleFunc("DELETE /backups/{cid}", s.requireAPIKey(s.handleBackupDelete))

	// Shared theme stylesheet.
	mux.HandleFunc("GET /static/styles.css", handleCSS)

	return logRequests(mux)
}

// storeUpload derives metadata, writes the bytes to the store, and records
// the metadata row.
func (s *Server) storeUpload(data []byte) (*Meta, error) {
	if len(data) == 0 {
		return nil, errors.New("empty upload")
	}
	m, err := DeriveMetadata(data)
	if err != nil {
		return nil, err
	}
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := s.store.Put(m.CID, data, m.Mime); err != nil {
		return nil, err
	}
	if err := upsertMeta(s.db, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ── HTML admin handlers ────────────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
		page = p
	}
	total, err := countMeta(s.db)
	if err != nil {
		slog.Error("count blobs", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	blobs, err := listMeta(s.db, pageSize, (page-1)*pageSize)
	if err != nil {
		slog.Error("list blobs", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	pages := (total + pageSize - 1) / pageSize
	s.render(w, "index.html", map[string]any{
		"Blobs":   blobs,
		"Total":   total,
		"Page":    page,
		"Pages":   pages,
		"HasPrev": page > 1,
		"HasNext": page < pages,
		"Prev":    page - 1,
		"Next":    page + 1,
	})
}

func (s *Server) handleUploadForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "upload.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

func (s *Server) handleAdminUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(s.maxUpload); err != nil {
		http.Redirect(w, r, "/upload?error=Upload+failed", http.StatusSeeOther)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Redirect(w, r, "/upload?error=No+file+selected", http.StatusSeeOther)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, s.maxUpload))
	if err != nil {
		http.Redirect(w, r, "/upload?error=Could+not+read+file", http.StatusSeeOther)
		return
	}
	if _, err := s.storeUpload(data); err != nil {
		http.Redirect(w, r, "/upload?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	if cid := r.PathValue("cid"); validCID(cid) {
		if _, err := deleteMeta(s.db, cid); err != nil {
			slog.Error("delete metadata", "cid", cid, "err", err)
		}
		if err := s.store.Delete(cid); err != nil {
			slog.Error("delete bytes", "cid", cid, "err", err)
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ── login handlers ─────────────────────────────────────────────────────────

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
		slog.Error("create session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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

// ── public JSON / bytes API ────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	total, err := countMeta(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read index")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "blobs", "ok": true, "blobs": total,
	})
}

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
		page = p
	}
	total, err := countMeta(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read index")
		return
	}
	blobs, err := listMeta(s.db, pageSize, (page-1)*pageSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list blobs")
		return
	}
	if blobs == nil {
		blobs = []Meta{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"blobs": blobs,
		"total": total,
		"page":  page,
		"pages": (total + pageSize - 1) / pageSize,
	})
}

func (s *Server) handleAPIGetBytes(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	etag := `"` + cid + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// Only blobs in the media index are publicly served — this keeps backup
	// snapshots (stored in R2 but not indexed) off the public endpoint.
	meta, err := getMeta(s.db, cid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read blob")
		return
	}
	if meta == nil {
		writeError(w, http.StatusNotFound, "blob not found")
		return
	}
	data, err := s.store.Get(cid)
	if err != nil {
		slog.Error("get bytes", "cid", cid, "err", err)
		writeError(w, http.StatusInternalServerError, "could not read blob")
		return
	}
	if data == nil {
		writeError(w, http.StatusNotFound, "blob not found")
		return
	}
	mime := meta.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("ETag", etag)
	// Content-addressed: the bytes for a CID never change.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

func (s *Server) handleAPIGetMeta(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	m, err := getMeta(s.db, cid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read metadata")
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "blob not found")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ── API-key-gated write API ────────────────────────────────────────────────

func (s *Server) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	}
	m, err := s.storeUpload(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		writeError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	existed, err := deleteMeta(s.db, cid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete blob")
		return
	}
	if err := s.store.Delete(cid); err != nil {
		slog.Error("delete bytes", "cid", cid, "err", err)
	}
	if !existed {
		writeError(w, http.StatusNotFound, "blob not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": cid})
}

// ── helpers ────────────────────────────────────────────────────────────────

func handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(w, theme.CSS)
}

// tmplFuncs are helpers available to every template.
var tmplFuncs = template.FuncMap{
	"humanSize": humanSize,
	"shortDate": shortDate,
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
		t, err := template.New("base.html").Funcs(tmplFuncs).
			ParseFS(assets, "templates/base.html", page)
		if err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
}

// humanSize formats a byte count as B / KB / MB.
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// shortDate trims an RFC3339 timestamp to its YYYY-MM-DD date portion.
func shortDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// render writes a page through base.html, buffering first so a template
// error never produces a half-written response.
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request",
			"method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
	})
}
