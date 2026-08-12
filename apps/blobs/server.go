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
	"path/filepath"
	"strconv"
	"strings"
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

const (
	defaultMaxUpload = 100 << 20 // 100 MiB
	pageSize         = 48        // blobs per admin page
	// uploadMemoryLimit is how much of a multipart form Go keeps on the heap
	// before spilling to a temp file. Small on purpose — blobs are media, and
	// every upload path here reads them from a file handle.
	uploadMemoryLimit = 1 << 20 // 1 MiB
)

// Server holds the running blob service.
type Server struct {
	db        *sql.DB
	store     ByteStore
	auth      *web.Auth
	rd        *web.Renderer
	maxUpload int64
	// spoolDir stages uploads on disk while they are hashed and inspected, so
	// a large blob never has to fit in memory.
	spoolDir string
	// sources locates the services whose bodies reference blobs — the
	// hygiene page scans them. See hygieneSources for the env vars.
	sources hygieneSources
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
		rd: &web.Renderer{Templates: tmpl, AssetVer: theme.Version, Funcs: tmplFuncs,
			App: "blobs", Mark: "bl",
			Nav: []web.NavItem{
				{Label: "Upload", URL: "/upload"},
				{Label: "Hygiene", URL: "/hygiene"},
				{Label: "Log out", URL: "/logout"},
			},
		},
		maxUpload: defaultMaxUpload,
		spoolDir:  store.Env("BLOBS_SPOOL_DIR", "blob-spool"),
		sources: hygieneSources{
			ContentURL: store.Env("CONTENT_URL", "http://127.0.0.1:8787"),
			ContentKey: store.Env("CONTENT_API_KEY", ""),
			FeedURL:    store.Env("FEED_URL", "http://127.0.0.1:8788"),
			FeedKey:    store.Env("FEED_READ_KEY", ""),
		},
	}

	s.pruneSpool() // reclaim upload spool files a crash stranded

	defer keys.Attach(s.auth, "blobs")() // admin-issued keys, when KEYS_DB_PATH is set

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
	mux.HandleFunc("GET /hygiene", s.auth.RequireSession(s.handleHygiene))
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

	// Gzip everywhere except the raw-byte routes: the admin HTML and the JSON
	// index compress well, while /blobs/<cid> and /backups/<cid> stream
	// already-compressed media and must reach ServeContent untouched so Range
	// requests keep working. Pulse traffic recording sits innermost so logged
	// timings stay real.
	rawBytes := web.PathPrefixSkipper("/backups/")
	handler := web.GzipExcept(s.pulse.Wrap(mux), func(r *http.Request) bool {
		// /blobs and /blobs/{cid}/meta are JSON; /blobs/{cid} is bytes.
		if strings.HasPrefix(r.URL.Path, "/blobs/") && !strings.HasSuffix(r.URL.Path, "/meta") {
			return true
		}
		return rawBytes(r)
	})
	return web.CORS(web.LogRequests(handler),
		"GET", "POST", "PUT", "DELETE", "OPTIONS")
}

// spoolTTL is how long a stranded spool file may sit before a startup sweep
// reclaims it. Well past any legitimate in-flight upload.
const spoolTTL = 6 * time.Hour

// pruneSpool removes upload spool files a crash stranded. Every live upload
// removes its own file by defer, so anything older than spoolTTL outlived the
// process that created it.
func (s *Server) pruneSpool() {
	entries, err := os.ReadDir(s.spoolDir)
	if err != nil {
		return // no spool directory yet — nothing to reclaim
	}
	cutoff := time.Now().Add(-spoolTTL)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "upload-") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(s.spoolDir, e.Name())
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("remove orphaned upload spool", "path", path, "err", err)
		}
	}
}

// inspectLimit is the size below which an upload is read into memory so its
// pixels can be decoded and thumbnailed. Above it — the videos and audio that
// make up the large end of the bucket, which never get a thumbnail anyway —
// the bytes go from the spool file straight to the backend and only the
// header is examined. It is what keeps a 100 MiB upload off the heap.
const inspectLimit = 32 << 20 // 32 MiB

// storeUploadFrom spools an upload to disk, then stores it. Nothing larger
// than inspectLimit is ever held in memory: the spool file is hashed in one
// streaming pass, deduped against what is already stored, and handed to the
// backend as a file.
func (s *Server) storeUploadFrom(src io.Reader) (*Meta, error) {
	if err := os.MkdirAll(s.spoolDir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(s.spoolDir, "upload-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	// Read one byte past the cap so an oversize upload is rejected rather
	// than silently truncated into a corrupt blob.
	size, err := io.Copy(tmp, io.LimitReader(src, s.maxUpload+1))
	if err != nil {
		return nil, err
	}
	if size > s.maxUpload {
		return nil, fmt.Errorf("upload exceeds %d byte limit", s.maxUpload)
	}
	if size == 0 {
		return nil, errors.New("empty upload")
	}

	// Small enough to inspect: take the in-memory path so images still get
	// their dimensions, palette, and thumbnail.
	if size <= inspectLimit {
		data := make([]byte, size)
		if _, err := tmp.ReadAt(data, 0); err != nil {
			return nil, err
		}
		return s.storeUpload(data)
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	blobCID, _, err := cid.OfReader(tmp)
	if err != nil {
		return nil, err
	}
	if existing, err := getMeta(s.db, blobCID); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil // content-addressed: already stored
	}

	// http.DetectContentType and the ftyp sniffer both read only the first
	// few hundred bytes, so a header slice classifies the file exactly as a
	// full read would.
	head := make([]byte, 512)
	n, err := tmp.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	m := Meta{
		CID:       blobCID,
		Size:      size,
		Mime:      sniffMime(head[:n]),
		CreatedAt: store.NowRFC3339(),
	}
	if err := s.store.PutFile(m.CID, tmp.Name(), m.Mime); err != nil {
		return nil, err
	}
	if err := upsertMeta(s.db, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// storeUpload derives metadata, writes the bytes to the store, generates a
// thumbnail for large images, and records the metadata row. Callers holding
// only a stream should use storeUploadFrom, which never buffers.
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
	// The argument is how much of the form to keep on the heap, NOT the
	// upload cap — passing maxUpload asked Go to hold a whole 100 MiB video
	// in memory. Past uploadMemoryLimit the part spills to a temp file, and
	// storeUploadFrom streams from there. The size cap is enforced below.
	if err := r.ParseMultipartForm(uploadMemoryLimit); err != nil {
		http.Redirect(w, r, "/upload?error=Upload+failed", http.StatusSeeOther)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Redirect(w, r, "/upload?error=No+file+selected", http.StatusSeeOther)
		return
	}
	defer file.Close()

	if _, err := s.storeUploadFrom(file); err != nil {
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
	// The hygiene page deletes orphans in place — send its deletes back to
	// it. Whitelisted, never echoed, so the target cannot be attacker-chosen.
	next := "/"
	if r.FormValue("next") == "/hygiene" {
		next = "/hygiene"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
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
	// Both backends hand back a seekable stream — an *os.File locally, a
	// rangeReader over R2 — so ServeContent can answer Range requests with
	// 206s (Safari refuses to play video without them). The copy branch is
	// the fallback for a stream of unknown length.
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
	// Meta is derived from the bytes at upload and never updated, so it is
	// as content-addressed as the bytes: same caching contract. This is what
	// lets the CDN edge answer repeat meta reads without riding the tunnel.
	etag := `"` + cid + `"`
	if web.ETagMatch(r, cid) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.WriteHeader(http.StatusNotModified)
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
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	web.WriteJSON(w, http.StatusOK, m)
}

// ── API-key-gated write API ────────────────────────────────────────────────

func (s *Server) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUpload)
	m, err := s.storeUploadFrom(r.Body)
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
	"mediaKind": mediaKind,
}

// mediaKind buckets a MIME type into the tag family a template should render
// it with: image, video, audio, or file for everything else.
func mediaKind(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	}
	return "file"
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
