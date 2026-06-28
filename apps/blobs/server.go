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
	"time"

	"github.com/iammatthias/farfield/lib/pulse"
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
	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder
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
			ReadKey:      store.Env("BLOBS_READ_KEY", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd:        &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
		maxUpload: defaultMaxUpload,
	}

	s.pulse = pulse.New(s.db, "blobs")
	defer s.pulse.Close()
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

	// Bytes and per-CID metadata stay public: images are embedded as <img> on
	// public pages and loaded by the browser, which cannot send a bearer, and a
	// CID is needed to reach them. The index LIST enumerates every stored CID,
	// so it is bearer-gated when BLOBS_READ_KEY is set. /status stays public.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /blobs", s.auth.RequireReadKey(s.handleAPIList))
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
	// and the immutable blob responses are better left untouched. Pulse traffic
	// recording sits innermost so logged timings stay real.
	return web.CORS(web.LogRequests(s.pulse.Wrap(mux)),
		"GET", "POST", "PUT", "DELETE", "OPTIONS")
}

// storeUpload derives metadata, writes the bytes to the store, generates a
// thumbnail for large images, and records the metadata row.
func (s *Server) storeUpload(data []byte) (*Meta, error) {
	if len(data) == 0 {
		return nil, errors.New("empty upload")
	}
	// Content-addressed: re-uploading known bytes is a no-op. Short-circuit
	// before the image decode and the backend PUT.
	if existing, err := getMeta(s.db, BlobCID(data)); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	m, img, err := deriveMetadata(data)
	if err != nil {
		return nil, err
	}
	if m.CreatedAt == "" {
		m.CreatedAt = store.NowRFC3339()
	}
	if err := s.store.Put(m.CID, data, m.Mime); err != nil {
		return nil, err
	}
	// Thumbnail large images at upload time so the admin grid never pulls
	// full-size originals. Failure is non-fatal: an empty thumb_cid falls
	// back to the full blob.
	if img != nil {
		if thumb := thumbJPEG(img); thumb != nil {
			tcid := BlobCID(thumb)
			if err := s.store.Put(tcid, thumb, "image/jpeg"); err != nil {
				slog.Error("store thumbnail", "cid", m.CID, "thumb", tcid, "err", err)
			} else {
				m.ThumbCID = tcid
			}
		}
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
		existed, thumb, err := deleteMeta(s.db, cid)
		if err != nil {
			slog.Error("delete metadata", "cid", cid, "err", err)
		} else if existed {
			if err := s.store.Delete(cid); err != nil {
				slog.Error("delete bytes", "cid", cid, "err", err)
			}
			s.deleteThumbBytes(thumb)
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// deleteThumbBytes drops a thumbnail's stored bytes once no remaining row
// references them — identical images dedupe to a shared thumb CID, so the
// bytes may still be live after one referencing blob is deleted.
func (s *Server) deleteThumbBytes(thumb string) {
	if thumb == "" {
		return
	}
	refs, err := thumbRefCount(s.db, thumb)
	if err != nil {
		slog.Error("count thumb refs", "thumb", thumb, "err", err)
		return
	}
	if refs > 0 {
		return
	}
	if err := s.store.Delete(thumb); err != nil {
		slog.Error("delete thumb bytes", "thumb", thumb, "err", err)
	}
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
	if web.ETagMatch(r, cid) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// Only blobs in the media index — or their generated thumbnails — are
	// publicly served. This keeps backup snapshots (stored in R2 but not
	// indexed) off the public endpoint.
	meta, err := getMeta(s.db, cid)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read blob")
		return
	}
	mime := "image/jpeg" // thumbnails are always JPEG
	if meta != nil {
		if mime = meta.Mime; mime == "" {
			mime = "application/octet-stream"
		}
	} else {
		refs, err := thumbRefCount(s.db, cid)
		if err != nil {
			web.WriteError(w, http.StatusInternalServerError, "could not read blob")
			return
		}
		if refs == 0 {
			web.WriteError(w, http.StatusNotFound, "blob not found")
			return
		}
	}
	body, size, err := s.store.GetStream(cid)
	if err != nil {
		slog.Error("get bytes", "cid", cid, "err", err)
		web.WriteError(w, http.StatusInternalServerError, "could not read blob")
		return
	}
	if body == nil {
		web.WriteError(w, http.StatusNotFound, "blob not found")
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", mime)
	w.Header().Set("ETag", etag)
	// Content-addressed: the bytes for a CID never change.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	// The local backend hands back an *os.File — let ServeContent stream it
	// with Range support and Content-Length. R2 bodies are not seekable, so
	// send the known size and copy.
	if rs, ok := body.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", time.Time{}, rs)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, body)
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
	existed, thumb, err := deleteMeta(s.db, cid)
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
	s.deleteThumbBytes(thumb)
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
