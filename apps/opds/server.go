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
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
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

const (
	defaultMaxUpload = 100 << 20 // 100 MiB
	pageSize         = 48        // books per admin page
)

// Server holds the running OPDS service.
type Server struct {
	db           *sql.DB
	store        ByteStore
	templates    map[string]*template.Template
	password     string
	apiKey       string
	cookieSecure bool
	maxUpload    int64
	assetVer     string // content hash of the stylesheet — cache-busts the URL
}

// openStore selects the byte-store backend from the environment.
func openStore() (ByteStore, string, error) {
	switch store.Env("OPDS_BACKEND", "local") {
	case "local":
		dir := store.Env("OPDS_DIR", "opds-data")
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
		return nil, "", fmt.Errorf(`OPDS_BACKEND must be "local" or "r2"`)
	}
}

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("OPDS_DB_PATH", "opds.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	bs, desc, err := openStore()
	if err != nil {
		return err
	}
	slog.Info("book store", "backend", desc)

	tmpl, err := parseTemplates()
	if err != nil {
		return err
	}

	s := &Server{
		db:           db,
		store:        bs,
		templates:    tmpl,
		password:     store.Env("PASSWORD", ""),
		apiKey:       store.Env("OPDS_API_KEY", ""),
		cookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		maxUpload:    defaultMaxUpload,
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
	mux.HandleFunc("GET /upload", s.requireSession(s.handleUploadForm))
	mux.HandleFunc("POST /upload", s.requireSession(s.handleAdminUpload))
	mux.HandleFunc("POST /upload/file", s.requireSession(s.handleUploadJSON))
	mux.HandleFunc("POST /books/{cid}/collection", s.requireSession(s.handleSetCollection))
	mux.HandleFunc("POST /books/{cid}/delete", s.requireSession(s.handleAdminDelete))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /logout", s.handleLogout)

	// OPDS catalog — session OR HTTP Basic (the credential e-readers send).
	// /opds is a navigation feed of folders; /opds/all and /opds/collection are
	// the acquisition feeds those folders link to.
	mux.HandleFunc("GET /opds", s.requireCatalogAuth(s.handleOPDSRoot))
	mux.HandleFunc("GET /opds/all", s.requireCatalogAuth(s.handleOPDSAll))
	mux.HandleFunc("GET /opds/collection", s.requireCatalogAuth(s.handleOPDSCollection))
	mux.HandleFunc("GET /opds/download/{cid}", s.requireCatalogAuth(s.handleDownload))
	mux.HandleFunc("GET /opds/cover/{cid}", s.requireCatalogAuth(s.handleCover))

	// JSON write API — API-key-gated.
	mux.HandleFunc("POST /api/books", s.requireAPIKey(s.handleUploadJSON))
	mux.HandleFunc("DELETE /api/books/{cid}", s.requireAPIKey(s.handleAPIDelete))

	// Public health + shared theme stylesheet.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /static/styles.css", handleCSS)

	return cors(logRequests(mux))
}

// storeUpload validates EPUB bytes, extracts metadata and the cover, writes
// both to the byte store, and records the book row. filename is the original
// upload name (best-effort) used for the download name and a title fallback.
func (s *Server) storeUpload(data []byte, filename, collection string) (*Book, error) {
	if len(data) == 0 {
		return nil, errors.New("empty upload")
	}
	meta, coverBytes, coverMime, err := parseEPUB(data)
	if err != nil {
		return nil, err
	}

	title := strings.TrimSpace(meta.Title)
	if title == "" {
		title = titleFromFilename(filename)
	}
	b := &Book{
		CID:         cid.Of(data),
		Title:       title,
		Author:      meta.Author,
		Language:    meta.Language,
		Identifier:  meta.Identifier,
		Description: meta.Description,
		Collection:  strings.TrimSpace(collection),
		Filename:    sanitizeFilename(filename),
		Size:        int64(len(data)),
		CreatedAt:   nowRFC3339(),
	}

	if len(coverBytes) > 0 {
		coverCID := cid.Of(coverBytes)
		if err := s.store.Put(coverCID, coverBytes, coverMime); err != nil {
			return nil, err
		}
		b.CoverCID = coverCID
		b.CoverMime = coverMime
	}
	if err := s.store.Put(b.CID, data, epubMime); err != nil {
		return nil, err
	}
	if err := upsertBook(s.db, b); err != nil {
		return nil, err
	}
	return b, nil
}

// deleteBookAndBytes removes a book row and its EPUB bytes, plus the cover
// bytes once no remaining book references them. It reports whether the book
// existed.
func (s *Server) deleteBookAndBytes(cid string) (bool, error) {
	b, err := deleteBook(s.db, cid)
	if err != nil {
		return false, err
	}
	if b == nil {
		return false, nil
	}
	if err := s.store.Delete(b.CID); err != nil {
		slog.Error("delete epub bytes", "cid", b.CID, "err", err)
	}
	if b.CoverCID != "" {
		if _, stillUsed, err := coverInfo(s.db, b.CoverCID); err == nil && !stillUsed {
			if err := s.store.Delete(b.CoverCID); err != nil {
				slog.Error("delete cover bytes", "cid", b.CoverCID, "err", err)
			}
		}
	}
	return true, nil
}

// ── HTML admin handlers ────────────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	named, uncategorized, err := collectionStats(s.db)
	if err != nil {
		slog.Error("collection stats", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	total := uncategorized
	for _, c := range named {
		total += c.Count
	}
	data := map[string]any{
		"Collections":   named,
		"Uncategorized": uncategorized,
		"Total":         total,
		"Self":          r.URL.RequestURI(),
		"Filter":        "",
		"Filtered":      false,
	}

	// Filtered view — all books in one collection, no pagination.
	if q.Has("collection") {
		name := q.Get("collection")
		books, err := listBooksByCollection(s.db, name)
		if err != nil {
			slog.Error("list collection", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		label := name
		if label == "" {
			label = "Uncategorized"
		}
		data["Books"] = books
		data["Filter"] = name
		data["Filtered"] = true
		data["FilterLabel"] = label
		s.render(w, "index.html", data)
		return
	}

	// All view — paginated.
	page := 1
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 1 {
		page = p
	}
	books, err := listBooks(s.db, pageSize, (page-1)*pageSize)
	if err != nil {
		slog.Error("list books", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	pages := (total + pageSize - 1) / pageSize
	data["Books"] = books
	data["Page"] = page
	data["Pages"] = pages
	data["HasPrev"] = page > 1
	data["HasNext"] = page < pages
	data["Prev"] = page - 1
	data["Next"] = page + 1
	s.render(w, "index.html", data)
}

func (s *Server) handleUploadForm(w http.ResponseWriter, r *http.Request) {
	named, _, _ := collectionStats(s.db) // a convenience datalist; ignore errors
	s.render(w, "upload.html", map[string]any{
		"Error":       r.URL.Query().Get("error"),
		"Collections": named,
	})
}

func (s *Server) handleAdminUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(s.maxUpload); err != nil {
		http.Redirect(w, r, "/upload?error=Upload+failed", http.StatusSeeOther)
		return
	}
	var files []*multipart.FileHeader
	if r.MultipartForm != nil {
		files = r.MultipartForm.File["file"]
	}
	if len(files) == 0 {
		http.Redirect(w, r, "/upload?error=No+file+selected", http.StatusSeeOther)
		return
	}

	// Bulk: process every selected file, tolerating per-file failures so one
	// bad EPUB doesn't sink the whole batch. The collection (if any) applies
	// to every file in the batch.
	collection := r.FormValue("collection")
	var stored int
	var failed []string
	for _, fh := range files {
		if err := s.storeMultipartFile(fh, collection); err != nil {
			slog.Error("upload: file failed", "name", fh.Filename, "err", err)
			failed = append(failed, fh.Filename)
			continue
		}
		stored++
	}
	if len(failed) > 0 {
		msg := fmt.Sprintf("Stored %d; %d failed: %s", stored, len(failed), strings.Join(failed, ", "))
		http.Redirect(w, r, "/upload?error="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// storeMultipartFile reads one uploaded file (bounded by maxUpload) and stores
// it as a book in the given collection.
func (s *Server) storeMultipartFile(fh *multipart.FileHeader, collection string) error {
	f, err := fh.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, s.maxUpload))
	if err != nil {
		return err
	}
	_, err = s.storeUpload(data, fh.Filename, collection)
	return err
}

func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	if cid := r.PathValue("cid"); validCID(cid) {
		if _, err := s.deleteBookAndBytes(cid); err != nil {
			slog.Error("delete book", "cid", cid, "err", err)
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleSetCollection moves a book into a collection (an empty value clears it).
func (s *Server) handleSetCollection(w http.ResponseWriter, r *http.Request) {
	if cid := r.PathValue("cid"); validCID(cid) {
		if _, err := updateBookCollection(s.db, cid, strings.TrimSpace(r.FormValue("collection"))); err != nil {
			slog.Error("set collection", "cid", cid, "err", err)
		}
	}
	// Return to the view they were on (validated to a local path).
	dest := r.FormValue("next")
	if !strings.HasPrefix(dest, "/") {
		dest = "/"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
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

// ── OPDS catalog ───────────────────────────────────────────────────────────

// handleOPDSRoot serves the navigation feed: an "All books" folder, one folder
// per collection, and "Uncategorized" when some books have none.
func (s *Server) handleOPDSRoot(w http.ResponseWriter, r *http.Request) {
	named, uncategorized, err := collectionStats(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read catalog")
		return
	}
	total := uncategorized
	for _, c := range named {
		total += c.Count
	}
	items := []NavItem{{Title: "All books", Href: "/opds/all", Count: total}}
	for _, c := range named {
		items = append(items, NavItem{
			Title: c.Name,
			Href:  "/opds/collection?c=" + url.QueryEscape(c.Name),
			Count: c.Count,
		})
	}
	if uncategorized > 0 {
		items = append(items, NavItem{Title: "Uncategorized", Href: "/opds/collection?c=", Count: uncategorized})
	}
	body, err := navFeedXML(items, "/opds", nowRFC3339())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not render catalog")
		return
	}
	w.Header().Set("Content-Type", navFeedType)
	_, _ = w.Write(body)
}

// handleOPDSAll serves the acquisition feed of every book.
func (s *Server) handleOPDSAll(w http.ResponseWriter, r *http.Request) {
	books, err := listBooks(s.db, -1, 0) // -1: every book, newest first
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read catalog")
		return
	}
	s.writeAcquisition(w, books, "farfield · opds — All books", "/opds/all")
}

// handleOPDSCollection serves one collection's acquisition feed (an empty c is
// the uncategorised books).
func (s *Server) handleOPDSCollection(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("c")
	books, err := listBooksByCollection(s.db, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read catalog")
		return
	}
	label := name
	if label == "" {
		label = "Uncategorized"
	}
	s.writeAcquisition(w, books, "farfield · opds — "+label, "/opds/collection?c="+url.QueryEscape(name))
}

// writeAcquisition renders and writes an OPDS acquisition feed.
func (s *Server) writeAcquisition(w http.ResponseWriter, books []Book, title, selfHref string) {
	updated := nowRFC3339()
	if len(books) > 0 {
		updated = books[0].CreatedAt
	}
	body, err := catalogXML(books, title, selfHref, updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not render catalog")
		return
	}
	w.Header().Set("Content-Type", feedType)
	_, _ = w.Write(body)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cid")
	if !validCID(id) {
		writeError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	etag := `"` + id + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	b, err := getBook(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read book")
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	data, err := s.store.Get(id)
	if err != nil {
		slog.Error("get epub bytes", "cid", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not read book")
		return
	}
	if data == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	w.Header().Set("Content-Type", epubMime)
	w.Header().Set("ETag", etag)
	// Content-addressed: the bytes for a CID never change.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Disposition", `attachment; filename="`+downloadName(b)+`"`)
	_, _ = w.Write(data)
}

func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cid")
	if !validCID(id) {
		writeError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	etag := `"` + id + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// Only serve covers that belong to a book — never arbitrary store keys.
	mime, ok, err := coverInfo(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read cover")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "cover not found")
		return
	}
	data, err := s.store.Get(id)
	if err != nil {
		slog.Error("get cover bytes", "cid", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not read cover")
		return
	}
	if data == nil {
		writeError(w, http.StatusNotFound, "cover not found")
		return
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

// ── single-EPUB JSON upload (shared) ────────────────────────────────────────

// handleUploadJSON stores one raw EPUB from the request body and returns the
// book as JSON. It backs both the API-key write endpoint (POST /api/books) and
// the session-gated POST /upload/file the admin progress uploader drives one
// file at a time — so each file reports success or failure on its own.
func (s *Server) handleUploadJSON(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	}
	// Filename is advisory — a query param or X-Filename header; the title
	// falls back to it only when the EPUB declares none. collection is optional.
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		filename = r.Header.Get("X-Filename")
	}
	b, err := s.storeUpload(data, filename, r.URL.Query().Get("collection"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cid")
	if !validCID(id) {
		writeError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	existed, err := s.deleteBookAndBytes(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete book")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ── public health ──────────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	total, err := countBooks(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read index")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "opds", "ok": true, "books": total,
	})
}

// ── helpers ────────────────────────────────────────────────────────────────

func handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(w, theme.CSS)
}

// sanitizeFilename reduces an upload's name to a safe basename: no directory
// components, no control characters, and no quotes or backslashes that could
// break a Content-Disposition header.
func sanitizeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, name)
	return strings.TrimSpace(name)
}

// titleFromFilename derives a display title from an upload name when the EPUB
// declares none: the basename without its .epub extension, underscores spaced.
func titleFromFilename(name string) string {
	name = sanitizeFilename(name)
	name = strings.TrimSuffix(name, path.Ext(name))
	name = strings.TrimSpace(strings.ReplaceAll(name, "_", " "))
	if name == "" {
		return "Untitled"
	}
	return name
}

// downloadName is the filename offered to a reader on download: the stored
// upload name, else a name built from the title, always ending in .epub.
func downloadName(b *Book) string {
	if n := sanitizeFilename(b.Filename); n != "" {
		return n
	}
	name := sanitizeFilename(b.Title)
	if name == "" {
		name = b.CID
	}
	if !strings.HasSuffix(strings.ToLower(name), ".epub") {
		name += ".epub"
	}
	return name
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

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// cors adds permissive CORS headers so a browser on another origin can read
// the public endpoints, and answers preflight requests.
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
