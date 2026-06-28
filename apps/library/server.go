package main

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
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

const defaultMaxUpload = 100 << 20 // 100 MiB

// Server holds the running OPDS service.
type Server struct {
	db    *sql.DB
	store ByteStore
	auth  *web.Auth
	rd    *web.Renderer
	// uploadKey is an optional second credential (LIBRARY_UPLOAD_KEY) accepted
	// only by the book-upload and regroup endpoints — a narrower key to hand a
	// helper that adds and organizes books but must not delete them or read the
	// catalog (which the full LIBRARY_API_KEY, doubling as the catalog password,
	// would grant). Empty disables it; the full key still works everywhere.
	uploadKey string
	maxUpload int64
}

// openStore selects the byte-store backend from the environment.
func openStore() (ByteStore, string, error) {
	switch store.Env("LIBRARY_BACKEND", "local") {
	case "local":
		dir := store.Env("LIBRARY_DIR", "library-data")
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
		return nil, "", fmt.Errorf(`LIBRARY_BACKEND must be "local" or "r2"`)
	}
}

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("LIBRARY_DB_PATH", "library.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	bs, desc, err := openStore()
	if err != nil {
		return err
	}
	slog.Info("book store", "backend", desc)

	tmpl, err := web.ParseTemplates(assets, tmplFuncs)
	if err != nil {
		return err
	}

	s := &Server{
		db:    db,
		store: bs,
		auth: &web.Auth{
			DB:           db,
			Password:     store.Env("PASSWORD", ""),
			APIKey:       store.Env("LIBRARY_API_KEY", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd:        &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
		uploadKey: store.Env("LIBRARY_UPLOAD_KEY", ""),
		maxUpload: defaultMaxUpload,
	}

	return web.Serve(host, port, s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// HTML admin UI — session-gated.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleIndex))
	mux.HandleFunc("GET /upload", s.auth.RequireSession(s.handleUploadForm))
	mux.HandleFunc("POST /upload", s.auth.RequireSession(s.handleAdminUpload))
	mux.HandleFunc("POST /upload/file", s.auth.RequireSession(s.handleUploadJSON))
	mux.HandleFunc("POST /books/collection", s.auth.RequireSession(s.handleBulkCollection))
	mux.HandleFunc("POST /books/{cid}/delete", s.auth.RequireSession(s.handleAdminDelete))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// OPDS catalog — session OR HTTP Basic (the credential e-readers send).
	// /opds is a navigation feed of folders; /opds/all and /opds/collection are
	// the acquisition feeds those folders link to.
	mux.HandleFunc("GET /opds", s.requireCatalogAuth(s.handleOPDSRoot))
	mux.HandleFunc("GET /opds/all", s.requireCatalogAuth(s.handleOPDSAll))
	mux.HandleFunc("GET /opds/collection", s.requireCatalogAuth(s.handleOPDSCollection))
	mux.HandleFunc("GET /opds/download/{cid}", s.requireCatalogAuth(s.handleDownload))
	mux.HandleFunc("GET /opds/cover/{cid}", s.requireCatalogAuth(s.handleCover))

	// JSON write API. Upload and regroup accept either the full LIBRARY_API_KEY
	// or the narrower LIBRARY_UPLOAD_KEY (the "intern" key). Delete stays on the
	// full key only, so the upload key can add and organize books but never
	// remove them.
	mux.HandleFunc("POST /api/books", s.requireUploadKey(s.handleUploadJSON))
	mux.HandleFunc("PUT /api/books/{cid}/collection", s.requireUploadKey(s.handleAPISetCollection))
	mux.HandleFunc("DELETE /api/books/{cid}", s.auth.RequireAPIKey(s.handleAPIDelete))

	// Public health + shared theme stylesheet.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// No Gzip: library's hot paths serve raw EPUB bytes and cover images,
	// which are already compressed. Pulse traffic recording sits innermost so
	// logged timings stay real.
	return web.CORS(web.LogRequests(pulse.Middleware(s.db, "library")(mux)),
		"GET", "POST", "PUT", "DELETE", "OPTIONS")
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
		CreatedAt:   store.NowRFC3339(),
	}

	if len(coverBytes) > 0 {
		coverCID := cid.Of(coverBytes)
		if err := s.store.Put(coverCID, coverBytes, coverMime); err != nil {
			return nil, err
		}
		b.CoverCID = coverCID
		b.CoverMime = coverMime
		// A small list-view thumbnail, stored under its own CID. Best-effort:
		// when the cover doesn't decode (or is already small) the full cover
		// serves in its place.
		if thumb := makeThumb(coverBytes); thumb != nil {
			thumbCID := cid.Of(thumb)
			if err := s.store.Put(thumbCID, thumb, "image/jpeg"); err != nil {
				slog.Error("store cover thumbnail", "cid", thumbCID, "err", err)
			} else {
				b.ThumbCID = thumbCID
			}
		}
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
// and thumbnail bytes once no remaining book references them. It reports
// whether the book existed.
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
	for _, imgCID := range []string{b.CoverCID, b.ThumbCID} {
		if imgCID == "" {
			continue
		}
		if _, stillUsed, err := coverInfo(s.db, imgCID); err == nil && !stillUsed {
			if err := s.store.Delete(imgCID); err != nil {
				slog.Error("delete cover bytes", "cid", imgCID, "err", err)
			}
		}
	}
	return true, nil
}

// ── HTML admin handlers ────────────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
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
		"Collections": named, // existing folders — for the move datalist
		"Total":       total,
		"Self":        r.URL.RequestURI(),
	}

	// Inside a named folder: just its books, with a breadcrumb back to root.
	if name := r.URL.Query().Get("collection"); name != "" {
		books, err := listBooksByCollection(s.db, name)
		if err != nil {
			slog.Error("list collection", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data["Root"] = false
		data["FolderName"] = name
		data["Books"] = books
		s.rd.Render(w, "index.html", data)
		return
	}

	// Root: the folders as rows at the top, then the unfoldered books — a file
	// browser. Folders are entries in the same list, not a separate page.
	loose, err := listBooksByCollection(s.db, "")
	if err != nil {
		slog.Error("list loose books", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data["Root"] = true
	data["Folders"] = named
	data["Books"] = loose
	s.rd.Render(w, "index.html", data)
}

func (s *Server) handleUploadForm(w http.ResponseWriter, r *http.Request) {
	named, _, _ := collectionStats(s.db) // a convenience datalist; ignore errors
	s.rd.Render(w, "upload.html", map[string]any{
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
	// Read one byte past the limit so an oversize file is detected and
	// rejected rather than silently truncated into a corrupt EPUB.
	data, err := io.ReadAll(io.LimitReader(f, s.maxUpload+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > s.maxUpload {
		return fmt.Errorf("%s: file too large", fh.Filename)
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

// handleBulkCollection moves the selected books into a collection. An empty
// value removes them from any folder. The folder is created implicitly — a
// collection exists exactly when some book names it.
func (s *Server) handleBulkCollection(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	collection := strings.TrimSpace(r.FormValue("collection"))
	cids := make([]string, 0, len(r.Form["cid"]))
	for _, cid := range r.Form["cid"] {
		if validCID(cid) {
			cids = append(cids, cid)
		}
	}
	// One UPDATE ... IN (...) for the whole selection, not a statement per book.
	if err := updateBooksCollection(s.db, cids, collection); err != nil {
		slog.Error("set collection", "count", len(cids), "err", err)
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
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

// ── OPDS catalog ───────────────────────────────────────────────────────────

// feedNotModified stamps the feed's cache validators — an ETag derived from
// the catalog version plus a short max-age — and reports whether the client's
// cached copy is still current, in which case it has already written the 304
// and the caller skips building the XML entirely.
func (s *Server) feedNotModified(w http.ResponseWriter, r *http.Request) bool {
	ver, err := catalogVersion(s.db)
	if err != nil {
		return false // no validators; build the feed as usual
	}
	w.Header().Set("ETag", `"`+ver+`"`)
	w.Header().Set("Cache-Control", "max-age=60")
	if web.ETagMatch(r, ver) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// handleOPDSRoot serves the navigation feed: an "All books" folder, one folder
// per collection, and "Uncategorized" when some books have none.
func (s *Server) handleOPDSRoot(w http.ResponseWriter, r *http.Request) {
	if s.feedNotModified(w, r) {
		return
	}
	named, uncategorized, err := collectionStats(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read catalog")
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
	body, err := navFeedXML(items, "/opds", store.NowRFC3339())
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not render catalog")
		return
	}
	w.Header().Set("Content-Type", navFeedType)
	_, _ = w.Write(body)
}

// handleOPDSAll serves the acquisition feed of every book.
func (s *Server) handleOPDSAll(w http.ResponseWriter, r *http.Request) {
	if s.feedNotModified(w, r) {
		return
	}
	books, err := listBooks(s.db) // every book, newest first
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read catalog")
		return
	}
	s.writeAcquisition(w, books, "farfield · library — All books", "/opds/all")
}

// handleOPDSCollection serves one collection's acquisition feed (an empty c is
// the uncategorised books).
func (s *Server) handleOPDSCollection(w http.ResponseWriter, r *http.Request) {
	if s.feedNotModified(w, r) {
		return
	}
	name := r.URL.Query().Get("c")
	books, err := listBooksByCollection(s.db, name)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read catalog")
		return
	}
	label := name
	if label == "" {
		label = "Uncategorized"
	}
	s.writeAcquisition(w, books, "farfield · library — "+label, "/opds/collection?c="+url.QueryEscape(name))
}

// writeAcquisition renders and writes an OPDS acquisition feed.
func (s *Server) writeAcquisition(w http.ResponseWriter, books []Book, title, selfHref string) {
	updated := store.NowRFC3339()
	if len(books) > 0 {
		updated = books[0].CreatedAt
	}
	body, err := catalogXML(books, title, selfHref, updated)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not render catalog")
		return
	}
	w.Header().Set("Content-Type", feedType)
	_, _ = w.Write(body)
}

// serveObject streams one content-addressed object from the byte store with
// its immutable cache headers. When the stream can seek (the local backend
// hands back an *os.File) it serves through http.ServeContent, which gets
// Range requests and Content-Length for free — the modtime is zero because
// content-addressed bytes never change and the ETag carries validation.
// Otherwise (R2's HTTP body) it sets Content-Length and copies.
func serveObject(w http.ResponseWriter, r *http.Request, rc io.ReadCloser, size int64) {
	if rs, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", time.Time{}, rs)
		return
	}
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	_, _ = io.Copy(w, rc)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cid")
	if !validCID(id) {
		web.WriteError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	// Content-addressed: a matching ETag means the client's copy is current,
	// before any database or byte-store work.
	if web.ETagMatch(r, id) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	b, err := getBook(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read book")
		return
	}
	if b == nil {
		web.WriteError(w, http.StatusNotFound, "book not found")
		return
	}
	rc, size, err := s.store.GetStream(id)
	if err != nil {
		slog.Error("get epub bytes", "cid", id, "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not read book")
		return
	}
	if rc == nil {
		web.WriteError(w, http.StatusNotFound, "book not found")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", epubMime)
	w.Header().Set("ETag", `"`+id+`"`)
	// Content-addressed: the bytes for a CID never change.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Disposition", `attachment; filename="`+downloadName(b)+`"`)
	serveObject(w, r, rc, size)
}

func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cid")
	if !validCID(id) {
		web.WriteError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	if web.ETagMatch(r, id) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// Only serve covers that belong to a book — never arbitrary store keys.
	mime, ok, err := coverInfo(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read cover")
		return
	}
	if !ok {
		web.WriteError(w, http.StatusNotFound, "cover not found")
		return
	}
	rc, size, err := s.store.GetStream(id)
	if err != nil {
		slog.Error("get cover bytes", "cid", id, "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not read cover")
		return
	}
	if rc == nil {
		web.WriteError(w, http.StatusNotFound, "cover not found")
		return
	}
	defer rc.Close()
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("ETag", `"`+id+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	serveObject(w, r, rc, size)
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
		web.WriteError(w, http.StatusRequestEntityTooLarge, "upload too large")
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
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	web.WriteJSON(w, http.StatusCreated, b)
}

// handleAPISetCollection regroups one existing book: it sets the book's
// collection to the `?collection=` value, or clears it (uncategorized) when the
// value is empty. This is the API path for organizing the library after upload —
// re-uploading the same EPUB keeps its original collection, so moving a book
// between folders goes through here. Folders are implicit: a collection exists
// exactly when some book names it.
func (s *Server) handleAPISetCollection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cid")
	if !validCID(id) {
		web.WriteError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	b, err := getBook(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read book")
		return
	}
	if b == nil {
		web.WriteError(w, http.StatusNotFound, "book not found")
		return
	}
	collection := strings.TrimSpace(r.URL.Query().Get("collection"))
	if err := updateBooksCollection(s.db, []string{id}, collection); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not set collection")
		return
	}
	b.Collection = collection
	web.WriteJSON(w, http.StatusOK, b)
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("cid")
	if !validCID(id) {
		web.WriteError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	existed, err := s.deleteBookAndBytes(id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete book")
		return
	}
	if !existed {
		web.WriteError(w, http.StatusNotFound, "book not found")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ── public health ──────────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	total, err := countBooks(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read index")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "library", "ok": true, "books": total,
	})
}

// ── helpers ────────────────────────────────────────────────────────────────

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
