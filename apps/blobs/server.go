package main

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

const (
	defaultMaxUpload = 100 << 20 // 100 MiB
	pageSize         = 48        // blobs per admin page
)

// Server holds the running blob service.
type Server struct {
	db        *sql.DB
	store     ByteStore
	auth      *web.Auth
	rd        *web.Renderer
	maxUpload int64
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
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	bs, desc, err := openStore()
	if err != nil {
		return err
	}
	slog.Info("blob store", "backend", desc)

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
			APIKey:       store.Env("BLOBS_API_KEY", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd:        &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
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
	mux.HandleFunc("POST /blobs/{cid}/delete", s.auth.RequireSession(s.handleAdminDelete))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// Public JSON / bytes API.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /blobs", s.handleAPIList)
	mux.HandleFunc("GET /blobs/{cid}", s.handleAPIGetBytes)
	mux.HandleFunc("GET /blobs/{cid}/meta", s.handleAPIGetMeta)

	// JSON write API — API-key-gated.
	mux.HandleFunc("POST /blobs", s.auth.RequireAPIKey(s.handleAPIUpload))
	mux.HandleFunc("DELETE /blobs/{cid}", s.auth.RequireAPIKey(s.handleAPIDelete))

	// Backup storage — API-key-gated opaque snapshots, kept out of the media index.
	mux.HandleFunc("POST /backups", s.auth.RequireAPIKey(s.handleBackupPut))
	mux.HandleFunc("GET /backups/{cid}", s.auth.RequireAPIKey(s.handleBackupGet))
	mux.HandleFunc("DELETE /backups/{cid}", s.auth.RequireAPIKey(s.handleBackupDelete))

	// Shared theme stylesheet.
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// No Gzip here: blobs serves raw, often already-compressed bytes (images),
	// and the immutable blob responses are better left untouched.
	return web.CORS(web.LogRequests(mux), "GET", "POST", "PUT", "DELETE", "OPTIONS")
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
		m.CreatedAt = store.NowRFC3339()
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
	s.rd.Render(w, "index.html", map[string]any{
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
	s.rd.Render(w, "upload.html", map[string]any{"Error": r.URL.Query().Get("error")})
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

	// Read one byte past the limit so an oversize file is detected and
	// rejected rather than silently truncated into a corrupt blob.
	data, err := io.ReadAll(io.LimitReader(file, s.maxUpload+1))
	if err != nil {
		http.Redirect(w, r, "/upload?error=Could+not+read+file", http.StatusSeeOther)
		return
	}
	if int64(len(data)) > s.maxUpload {
		http.Redirect(w, r, "/upload?error=File+too+large", http.StatusSeeOther)
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
		// Only drop the stored bytes when a metadata row actually existed:
		// backup snapshots share the bucket but have no meta row, and must
		// not be deletable through the media routes.
		existed, err := deleteMeta(s.db, cid)
		if err != nil {
			slog.Error("delete metadata", "cid", cid, "err", err)
		} else if existed {
			if err := s.store.Delete(cid); err != nil {
				slog.Error("delete bytes", "cid", cid, "err", err)
			}
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ── login handlers ─────────────────────────────────────────────────────────

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

// ── public JSON / bytes API ────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	total, err := countMeta(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read index")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
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
		web.WriteError(w, http.StatusInternalServerError, "could not read index")
		return
	}
	blobs, err := listMeta(s.db, pageSize, (page-1)*pageSize)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list blobs")
		return
	}
	if blobs == nil {
		blobs = []Meta{}
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"blobs": blobs,
		"total": total,
		"page":  page,
		"pages": (total + pageSize - 1) / pageSize,
	})
}

func (s *Server) handleAPIGetBytes(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		web.WriteError(w, http.StatusBadRequest, "malformed cid")
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
		web.WriteError(w, http.StatusInternalServerError, "could not read blob")
		return
	}
	if meta == nil {
		web.WriteError(w, http.StatusNotFound, "blob not found")
		return
	}
	data, err := s.store.Get(cid)
	if err != nil {
		slog.Error("get bytes", "cid", cid, "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not read blob")
		return
	}
	if data == nil {
		web.WriteError(w, http.StatusNotFound, "blob not found")
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
		web.WriteError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	m, err := getMeta(s.db, cid)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read metadata")
		return
	}
	if m == nil {
		web.WriteError(w, http.StatusNotFound, "blob not found")
		return
	}
	web.WriteJSON(w, http.StatusOK, m)
}

// ── API-key-gated write API ────────────────────────────────────────────────

func (s *Server) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		web.WriteError(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	}
	m, err := s.storeUpload(data)
	if err != nil {
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	web.WriteJSON(w, http.StatusCreated, m)
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		web.WriteError(w, http.StatusBadRequest, "malformed cid")
		return
	}
	existed, err := deleteMeta(s.db, cid)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete blob")
		return
	}
	if !existed {
		// No meta row means this CID was never a media blob — it may be a
		// backup snapshot sharing the bucket, so leave the bytes alone.
		web.WriteError(w, http.StatusNotFound, "blob not found")
		return
	}
	if err := s.store.Delete(cid); err != nil {
		slog.Error("delete bytes", "cid", cid, "err", err)
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": cid})
}

// ── helpers ────────────────────────────────────────────────────────────────

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
