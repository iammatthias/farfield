package main

import (
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

// maxIPABytes bounds an upload. Ad-hoc app archives run tens to a few hundred
// MiB; 2 GiB is generous headroom and rejects a runaway stream.
const maxIPABytes = 2 << 30

// Server holds the running sideload service.
type Server struct {
	db        *sql.DB
	auth      *web.Auth
	rd        *web.Renderer
	blobs     *blobStore
	publicURL string        // absolute HTTPS base for manifest/ipa/icon URLs
	limiter   *tokenLimiter // failed token lookups, per client IP
	ownerUDID string        // SIDELOAD_OWNER_UDID — kept in every app's whitelist

	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder
}

// run wires up dependencies and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("SIDELOAD_DB_PATH", "sideload.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	blobs, err := newBlobStore(store.Env("SIDELOAD_DIR", "sideload-blobs"))
	if err != nil {
		return err
	}

	s := newServer(db, blobs,
		store.Env("PASSWORD", ""),
		store.Env("SIDELOAD_API_KEY", ""),
		store.Env("COOKIE_SECURE", "false") == "true",
		store.Env("SIDELOAD_PUBLIC_URL", "http://"+net.JoinHostPort("127.0.0.1", port)))
	if owner, ok := normalizeUDID(store.Env("SIDELOAD_OWNER_UDID", "")); ok {
		s.ownerUDID = owner
		slog.Info("owner device pinned to every app whitelist")
	} else if raw := store.Env("SIDELOAD_OWNER_UDID", ""); raw != "" {
		slog.Warn("SIDELOAD_OWNER_UDID is not a valid UDID — ignoring", "value", raw)
	}
	if err := s.parseTemplates(); err != nil {
		return err
	}

	go s.sweepLoop()

	s.pulse = pulse.New(s.db, "sideload")
	defer s.pulse.Close()
	return web.Serve(host, port, s.routes())
}

func newServer(db *sql.DB, blobs *blobStore, password, apiKey string, cookieSecure bool, publicURL string) *Server {
	return &Server{
		db:    db,
		blobs: blobs,
		auth: &web.Auth{
			DB:           db,
			Password:     password,
			APIKey:       apiKey,
			CookieSecure: cookieSecure,
		},
		publicURL: strings.TrimRight(publicURL, "/"),
		limiter:   newTokenLimiter(20, time.Minute),
	}
}

func (s *Server) parseTemplates() error {
	tmpl, err := web.ParseTemplates(assets, template.FuncMap{
		"relAge":   relAge,
		"sizeText": sizeText,
		"markdown": renderMarkdown,
	})
	if err != nil {
		return err
	}
	s.rd = &web.Renderer{Templates: tmpl, AssetVer: theme.Version}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Author UI — session-gated.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleIndex))
	mux.HandleFunc("POST /upload", s.auth.RequireSession(s.handleUpload))
	mux.HandleFunc("GET /app/{bundle}", s.auth.RequireSession(s.handleApp))
	mux.HandleFunc("GET /app/{bundle}/edit", s.auth.RequireSession(s.handleAppEdit))
	mux.HandleFunc("POST /app/{bundle}/meta", s.auth.RequireSession(s.handleAppMetaSave))
	mux.HandleFunc("POST /app/{bundle}/screenshots", s.auth.RequireSession(s.handleScreenshotUpload))
	mux.HandleFunc("POST /app/{bundle}/screenshots/{sid}/caption", s.auth.RequireSession(s.handleScreenshotCaption))
	mux.HandleFunc("POST /app/{bundle}/screenshots/{sid}/move", s.auth.RequireSession(s.handleScreenshotMove))
	mux.HandleFunc("POST /app/{bundle}/screenshots/{sid}/delete", s.auth.RequireSession(s.handleScreenshotDelete))
	mux.HandleFunc("GET /app/{bundle}/devices", s.auth.RequireSession(s.handleDevices))
	mux.HandleFunc("POST /app/{bundle}/devices", s.auth.RequireSession(s.handleDeviceAdd))
	mux.HandleFunc("GET /app/{bundle}/devices.txt", s.auth.RequireSession(s.handleDevicesExport))
	mux.HandleFunc("POST /app/{bundle}/devices/import", s.auth.RequireSession(s.handleDeviceImport))
	mux.HandleFunc("POST /app/{bundle}/devices/{did}/delete", s.auth.RequireSession(s.handleDeviceDelete))
	mux.HandleFunc("POST /app/{bundle}/register/enable", s.auth.RequireSession(s.handleRegEnable))
	mux.HandleFunc("POST /app/{bundle}/register/disable", s.auth.RequireSession(s.handleRegDisable))
	mux.HandleFunc("POST /app/{bundle}/delete", s.auth.RequireSession(s.handleAppDelete))
	mux.HandleFunc("GET /b/{id}", s.auth.RequireSession(s.handleBuild))
	mux.HandleFunc("POST /b/{id}/share", s.auth.RequireSession(s.handleShareCreate))
	mux.HandleFunc("POST /b/{id}/notes", s.auth.RequireSession(s.handleBuildNotes))
	mux.HandleFunc("POST /b/{id}/delete", s.auth.RequireSession(s.handleDelete))
	mux.HandleFunc("GET /shares", s.auth.RequireSession(s.handleShares))
	mux.HandleFunc("POST /shares/{token}/revoke", s.auth.RequireSession(s.handleShareRevoke))

	// Login.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// Install session — token-gated, NO cookie (the iOS install daemon fetches
	// these). Literal final segments outrank one another cleanly under {token}.
	mux.HandleFunc("GET /i/{token}/manifest.plist", s.handleManifest)
	mux.HandleFunc("GET /i/{token}/app.ipa", s.handleIPA)
	mux.HandleFunc("GET /i/{token}/display.png", s.handleIcon(57))
	mux.HandleFunc("GET /i/{token}/full.png", s.handleIcon(512))

	// Public share landing + screenshot images (content-addressed, no token —
	// they appear on the public share page).
	mux.HandleFunc("GET /s/{token}", s.handleShareLanding)
	mux.HandleFunc("GET /shots/{sid}", s.handleScreenshot)

	// Public device registration — opt-in per app, reached by its token. The
	// .mobileconfig asks iOS to POST its UDID to the capture callback.
	mux.HandleFunc("GET /register/{token}", s.handleRegisterLanding)
	mux.HandleFunc("GET /register/{token}/enroll.mobileconfig", s.handleEnrollProfile)
	mux.HandleFunc("POST /register/{token}/capture", s.handleEnrollCapture)
	mux.HandleFunc("POST /register/{token}/submit", s.handleRegisterSubmit)
	mux.HandleFunc("GET /register/{token}/done", s.handleRegisterDone)

	// Agent API — X-API-Key.
	mux.HandleFunc("POST /api/builds", s.auth.RequireAPIKey(s.handleAPIUpload))
	mux.HandleFunc("GET /api/builds", s.auth.RequireAPIKey(s.handleAPIList))
	mux.HandleFunc("DELETE /api/builds/{id}", s.auth.RequireAPIKey(s.handleAPIDelete))
	mux.HandleFunc("POST /api/builds/{id}/share", s.auth.RequireAPIKey(s.handleAPIShare))
	mux.HandleFunc("DELETE /api/apps/{bundle}", s.auth.RequireAPIKey(s.handleAPIAppDelete))

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// Gzip self-skips octet-stream and Range, so wrapping the whole mux leaves
	// .ipa byte serving untouched while compressing HTML/JSON. Logging sits
	// outside for the final status; pulse innermost so timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(s.pulse.Wrap(mux))),
		"GET", "POST", "DELETE", "OPTIONS")
}

// sweepLoop prunes dead share tokens hourly (and once at startup).
func (s *Server) sweepLoop() {
	for {
		if n, err := pruneTokens(s.db); err != nil {
			slog.Warn("token sweep failed", "err", err)
		} else if n > 0 {
			slog.Info("token sweep", "pruned", n)
		}
		time.Sleep(time.Hour)
	}
}

// ── validation ───────────────────────────────────────────────────────────────

// idPattern is the short content-address shape: 16 base32 chars.
var idPattern = regexp.MustCompile(`^[a-z2-7]{16}$`)

// tokenPattern is the install-token shape: 26 base32 chars (auth.NewSessionToken).
var tokenPattern = regexp.MustCompile(`^[A-Z2-7]{26}$`)

func validID(id string) bool { return idPattern.MatchString(id) }

// ── upload / ingest ──────────────────────────────────────────────────────────

// ingest stores an uploaded .ipa under its content address, parses its
// metadata, and records the build. It streams the upload to disk (never
// buffering the whole archive) and dedupes identical bytes.
func (s *Server) ingest(src io.Reader, filename, gitCommit, notes string) (*Build, error) {
	fullCID, size, err := s.blobs.spool(src, maxIPABytes, ".ipa")
	if err != nil {
		return nil, err
	}
	id := fullCID[:16]

	meta, err := parseIPA(s.blobs.path(fullCID, ".ipa"))
	if err != nil {
		// Not a valid .ipa. Drop the bytes unless a prior build already claims
		// this content address.
		if existing, gerr := getBuild(s.db, id); gerr == nil && existing == nil {
			_ = s.blobs.remove(fullCID, ".ipa")
		}
		return nil, err
	}

	expiry := ""
	if !meta.ProfileExpiry.IsZero() {
		expiry = meta.ProfileExpiry.UTC().Format(time.RFC3339)
	}
	b := &Build{
		ID:            id,
		CID:           fullCID,
		BundleID:      meta.BundleID,
		AppName:       meta.AppName,
		Version:       meta.Version,
		BuildNumber:   meta.BuildNumber,
		Team:          meta.Team,
		ProfileExpiry: expiry,
		DeviceCount:   len(meta.UDIDs),
		UDIDs:         strings.Join(meta.UDIDs, "\n"),
		SizeBytes:     size,
		Filename:      sanitizeFilename(filename),
		GitCommit:     strings.TrimSpace(gitCommit),
		Notes:         strings.TrimSpace(notes),
	}
	created, err := insertBuild(s.db, b)
	if err != nil {
		return nil, err
	}
	// Keep the author's own device in the whitelist for every app.
	s.ensureOwnerDevice(b.BundleID)
	if !created {
		// Identical bytes already stored — return the canonical existing row.
		if existing, err := getBuild(s.db, id); err == nil && existing != nil {
			return existing, nil
		}
	}
	slog.Info("build ingested", "id", b.ID, "bundle", b.BundleID,
		"version", b.Version, "build", b.BuildNumber, "size", b.SizeBytes, "new", created)
	return b, nil
}

// readUpload extracts the .ipa reader and filename from a request, handling both
// a raw body (agent: curl --data-binary @app.ipa) and a multipart form field
// named "ipa" (browser). The returned closer, if non-nil, must be closed.
func readUpload(r *http.Request) (src io.Reader, filename string, closer io.Closer, err error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		f, hdr, ferr := r.FormFile("ipa")
		if ferr != nil {
			return nil, "", nil, fmt.Errorf("no .ipa file in form: %w", ferr)
		}
		return f, hdr.Filename, f, nil
	}
	name := r.URL.Query().Get("filename")
	if name == "" {
		name = "app.ipa"
	}
	return r.Body, name, nil, nil
}

// handleUpload is the browser multipart upload from the index form.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	src, filename, closer, err := readUpload(r)
	if err != nil {
		s.renderIndex(w, http.StatusBadRequest, err.Error())
		return
	}
	if closer != nil {
		defer closer.Close()
	}
	b, err := s.ingest(src, filename, r.FormValue("commit"), r.FormValue("notes"))
	if err != nil {
		s.renderIndex(w, http.StatusBadRequest, err.Error())
		return
	}
	http.Redirect(w, r, "/b/"+b.ID, http.StatusSeeOther)
}

// handleAPIUpload is the agent upload endpoint.
func (s *Server) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	src, filename, closer, err := readUpload(r)
	if err != nil {
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if closer != nil {
		defer closer.Close()
	}
	q := r.URL.Query()
	b, err := s.ingest(src, filename, q.Get("commit"), q.Get("notes"))
	if err != nil {
		web.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	web.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":            b.ID,
		"cid":           b.CID,
		"bundleId":      b.BundleID,
		"appName":       b.AppName,
		"version":       b.Version,
		"buildNumber":   b.BuildNumber,
		"profileExpiry": b.ProfileExpiry,
		"deviceCount":   b.DeviceCount,
		"sizeBytes":     b.SizeBytes,
		"installURL":    s.publicURL + "/b/" + b.ID,
	})
}

// ── author UI ────────────────────────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.renderIndex(w, http.StatusOK, "")
}

func (s *Server) renderIndex(w http.ResponseWriter, status int, errMsg string) {
	apps, err := listApps(s.db)
	if err != nil {
		slog.Error("list apps", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	total, _ := countBuilds(s.db)
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.rd.Render(w, "index.html", map[string]any{
		"Apps":  apps,
		"Total": total,
		"Error": errMsg,
	})
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	builds, err := listBuildsByBundle(s.db, bundle)
	if err != nil {
		slog.Error("list builds", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(builds) == 0 {
		http.NotFound(w, r)
		return
	}
	meta, shots, err := s.loadAppContent(bundle)
	if err != nil {
		slog.Error("load app content", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	devs, err := s.appDevices(bundle)
	if err != nil {
		slog.Error("list devices", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	prov := provisionedSet(&builds[0])
	pending := 0
	for _, d := range devs {
		if !prov[strings.ToLower(d.UDID)] {
			pending++
		}
	}
	s.rd.Render(w, "app.html", map[string]any{
		"BundleID":    bundle,
		"AppName":     builds[0].AppName,
		"Latest":      builds[0],
		"Builds":      builds,
		"Meta":        meta,
		"Screenshots": shots,
		"DeviceCount": len(devs),
		"Pending":     pending,
	})
}

// loadAppContent fetches an app's optional rich-page metadata and screenshots.
// Both may be empty — the page renders fine without them.
func (s *Server) loadAppContent(bundle string) (*AppMeta, []Screenshot, error) {
	meta, err := getAppMeta(s.db, bundle)
	if err != nil {
		return nil, nil, err
	}
	shots, err := listScreenshots(s.db, bundle)
	if err != nil {
		return nil, nil, err
	}
	return meta, shots, nil
}

// handleAppEdit renders the rich-page editor: tagline + description, and the
// screenshot manager.
func (s *Server) handleAppEdit(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	builds, err := listBuildsByBundle(s.db, bundle)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(builds) == 0 {
		http.NotFound(w, r)
		return
	}
	meta, shots, err := s.loadAppContent(bundle)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.rd.Render(w, "app_edit.html", map[string]any{
		"BundleID":    bundle,
		"AppName":     builds[0].AppName,
		"Meta":        meta,
		"Screenshots": shots,
		"Error":       r.URL.Query().Get("error"),
	})
}

// handleAppMetaSave stores the tagline and description (markdown). Clearing both
// removes the row, returning the app to the plain view.
func (s *Server) handleAppMetaSave(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	_ = r.ParseForm()
	if err := upsertAppMeta(s.db, &AppMeta{
		BundleID:    bundle,
		Tagline:     strings.TrimSpace(r.FormValue("tagline")),
		Description: strings.TrimSpace(r.FormValue("description")),
	}); err != nil {
		slog.Error("save app meta", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/edit", http.StatusSeeOther)
}

// handleScreenshotUpload stores an uploaded image for the app's gallery.
func (s *Server) handleScreenshotUpload(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	f, _, err := r.FormFile("image")
	if err != nil {
		s.editError(w, r, bundle, "choose an image to upload")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxScreenshotBytes+1))
	if err != nil {
		s.editError(w, r, bundle, "could not read the upload")
		return
	}
	if int64(len(data)) > maxScreenshotBytes {
		s.editError(w, r, bundle, "image too large (max 12 MB)")
		return
	}
	mime, ext, width, height, err := imageInfo(data)
	if err != nil {
		s.editError(w, r, bundle, err.Error())
		return
	}
	cidStr, err := s.blobs.putBytes(data, ext)
	if err != nil {
		slog.Error("store screenshot", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := addScreenshot(s.db, &Screenshot{
		ID:       store.ShortID(),
		BundleID: bundle,
		CID:      cidStr,
		Ext:      ext,
		Mime:     mime,
		Width:    width,
		Height:   height,
		Caption:  strings.TrimSpace(r.FormValue("caption")),
	}); err != nil {
		slog.Error("add screenshot", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/edit", http.StatusSeeOther)
}

// editError re-shows the edit page with a message.
func (s *Server) editError(w http.ResponseWriter, r *http.Request, bundle, msg string) {
	http.Redirect(w, r, "/app/"+bundle+"/edit?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) handleScreenshotCaption(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	_ = r.ParseForm()
	if err := setScreenshotCaption(s.db, r.PathValue("sid"), strings.TrimSpace(r.FormValue("caption"))); err != nil {
		slog.Error("caption", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/edit", http.StatusSeeOther)
}

func (s *Server) handleScreenshotMove(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	dir := r.URL.Query().Get("dir")
	if dir != "up" && dir != "down" {
		dir = "down"
	}
	if err := moveScreenshot(s.db, r.PathValue("sid"), dir); err != nil {
		slog.Error("move screenshot", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/edit", http.StatusSeeOther)
}

func (s *Server) handleScreenshotDelete(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	sh, err := deleteScreenshot(s.db, r.PathValue("sid"))
	if err != nil {
		slog.Error("delete screenshot", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sh != nil {
		// Drop the image file only when no other screenshot shares its bytes.
		if others, _ := screenshotsWithCID(s.db, sh.CID); others == 0 {
			if err := s.blobs.remove(sh.CID, sh.Ext); err != nil {
				slog.Warn("could not remove screenshot file", "cid", sh.CID, "err", err)
			}
		}
	}
	http.Redirect(w, r, "/app/"+bundle+"/edit", http.StatusSeeOther)
}

// handleScreenshot serves a screenshot image by id — public and immutable,
// since the bytes are content-addressed. It appears on the public share page.
func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	sh, err := getScreenshot(s.db, r.PathValue("sid"))
	if err != nil || sh == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("ETag", `"`+sh.CID+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if web.ETagMatch(r, sh.CID) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f, _, err := s.blobs.open(sh.CID, sh.Ext)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", sh.Mime)
	modtime := time.Time{}
	http.ServeContent(w, r, "screenshot"+sh.Ext, modtime, f)
}

// handleBuildNotes updates a version's changelog ("what's new").
func (s *Server) handleBuildNotes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := getBuild(s.db, id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	if err := updateBuildNotes(s.db, id, strings.TrimSpace(r.FormValue("notes"))); err != nil {
		slog.Error("update notes", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/b/"+id, http.StatusSeeOther)
}

// ── device whitelist (admin) ─────────────────────────────────────────────────

// deviceView pairs a whitelisted device with whether the latest build's
// profile already provisions it.
type deviceView struct {
	Device
	Provisioned bool
}

// ensureOwnerDevice keeps the configured owner UDID in an app's whitelist, so
// the author's own device is always provisioned in the next build. Idempotent.
func (s *Server) ensureOwnerDevice(bundle string) {
	if s.ownerUDID == "" {
		return
	}
	if _, err := addOrUpdateDevice(s.db, &Device{
		ID: store.ShortID(), BundleID: bundle, UDID: s.ownerUDID,
		Name: "me", Source: "owner",
	}); err != nil {
		slog.Warn("ensure owner device", "bundle", bundle, "err", err)
	}
}

// appDevices ensures the owner device is present, then lists an app's devices.
func (s *Server) appDevices(bundle string) ([]Device, error) {
	s.ensureOwnerDevice(bundle)
	return listDevices(s.db, bundle)
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	builds, err := listBuildsByBundle(s.db, bundle)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(builds) == 0 {
		http.NotFound(w, r)
		return
	}
	latest := builds[0]
	devs, err := s.appDevices(bundle)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	prov := provisionedSet(&latest)
	views := make([]deviceView, len(devs))
	pending := 0
	for i, d := range devs {
		p := prov[strings.ToLower(d.UDID)]
		views[i] = deviceView{Device: d, Provisioned: p}
		if !p {
			pending++
		}
	}
	reg, err := getRegistration(s.db, bundle)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	registerURL := ""
	if reg != nil {
		registerURL = s.publicURL + "/register/" + reg.Token
	}
	s.rd.Render(w, "devices.html", map[string]any{
		"BundleID":     bundle,
		"AppName":      latest.AppName,
		"Latest":       latest,
		"Devices":      views,
		"Total":        len(devs),
		"Pending":      pending,
		"ProfileCount": len(prov),
		"Registration": reg,
		"RegisterURL":  registerURL,
		"Export":       exportDevices(devs),
		"Error":        r.URL.Query().Get("error"),
	})
}

func (s *Server) devicesError(w http.ResponseWriter, r *http.Request, bundle, msg string) {
	http.Redirect(w, r, "/app/"+bundle+"/devices?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) handleDeviceAdd(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	_ = r.ParseForm()
	udid, ok := normalizeUDID(r.FormValue("udid"))
	if !ok {
		s.devicesError(w, r, bundle, "That doesn't look like a UDID (expected 25- or 40-char hex).")
		return
	}
	if _, err := addOrUpdateDevice(s.db, &Device{
		ID:       store.ShortID(),
		BundleID: bundle,
		UDID:     udid,
		Name:     strings.TrimSpace(r.FormValue("name")),
		Note:     strings.TrimSpace(r.FormValue("note")),
		Source:   "manual",
	}); err != nil {
		slog.Error("add device", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/devices", http.StatusSeeOther)
}

// handleDeviceImport seeds the whitelist from the latest build's profile — the
// devices already provisioned — so an existing app starts tracked.
func (s *Server) handleDeviceImport(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	builds, err := listBuildsByBundle(s.db, bundle)
	if err != nil || len(builds) == 0 {
		http.NotFound(w, r)
		return
	}
	n := 0
	for _, raw := range strings.Split(builds[0].UDIDs, "\n") {
		udid, ok := normalizeUDID(raw)
		if !ok {
			continue
		}
		if isNew, err := addOrUpdateDevice(s.db, &Device{
			ID: store.ShortID(), BundleID: bundle, UDID: udid, Source: "manual",
		}); err == nil && isNew {
			n++
		}
	}
	slog.Info("imported profile devices", "bundle", bundle, "added", n)
	http.Redirect(w, r, "/app/"+bundle+"/devices", http.StatusSeeOther)
}

func (s *Server) handleDeviceDelete(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	if err := deleteDevice(s.db, bundle, r.PathValue("did")); err != nil {
		slog.Error("delete device", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/devices", http.StatusSeeOther)
}

func (s *Server) handleDevicesExport(w http.ResponseWriter, r *http.Request) {
	devs, err := s.appDevices(r.PathValue("bundle"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="devices.txt"`)
	_, _ = io.WriteString(w, exportDevices(devs))
}

func (s *Server) handleRegEnable(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	if builds, err := listBuildsByBundle(s.db, bundle); err != nil || len(builds) == 0 {
		http.NotFound(w, r)
		return
	}
	if _, err := enableRegistration(s.db, bundle); err != nil {
		slog.Error("enable registration", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/devices", http.StatusSeeOther)
}

func (s *Server) handleRegDisable(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	if err := disableRegistration(s.db, bundle); err != nil {
		slog.Error("disable registration", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/app/"+bundle+"/devices", http.StatusSeeOther)
}

// ── public device registration ───────────────────────────────────────────────

// regBundle resolves the registration token to its bundle id, rate-limiting and
// rendering the gone page when invalid. ok=false means the response is written.
func (s *Server) regBundle(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := r.PathValue("token")
	if !tokenPattern.MatchString(token) {
		s.renderRegGone(w)
		return "", false
	}
	ip := clientIP(r)
	if s.limiter.blocked(ip) {
		s.renderRegGone(w)
		return "", false
	}
	bundle, err := registrationBundle(s.db, token)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", false
	}
	if bundle == "" {
		s.limiter.fail(ip)
		s.renderRegGone(w)
		return "", false
	}
	return bundle, true
}

func (s *Server) renderRegGone(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	s.rd.Render(w, "gone.html", nil)
}

// appName returns an app's friendly name from its latest build.
func (s *Server) appName(bundle string) string {
	builds, _ := listBuildsByBundle(s.db, bundle)
	if len(builds) > 0 && builds[0].AppName != "" {
		return builds[0].AppName
	}
	return bundle
}

func (s *Server) handleRegisterLanding(w http.ResponseWriter, r *http.Request) {
	bundle, ok := s.regBundle(w, r)
	if !ok {
		return
	}
	s.rd.Render(w, "register.html", map[string]any{
		"Token":    r.PathValue("token"),
		"AppName":  s.appName(bundle),
		"BundleID": bundle,
		"Error":    r.URL.Query().Get("error"),
	})
}

func (s *Server) handleEnrollProfile(w http.ResponseWriter, r *http.Request) {
	bundle, ok := s.regBundle(w, r)
	if !ok {
		return
	}
	token := r.PathValue("token")
	callback := s.publicURL + "/register/" + token + "/capture"
	mc, err := buildEnrollProfile(token, s.appName(bundle), callback)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-apple-aspen-config; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="enroll.mobileconfig"`)
	_, _ = w.Write(mc)
}

// handleEnrollCapture receives the signed device-attributes plist iOS posts
// after the user installs the enrolment profile, and whitelists the UDID.
func (s *Server) handleEnrollCapture(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	bundle, err := registrationBundle(s.db, token)
	if err != nil || bundle == "" {
		http.Error(w, "registration closed", http.StatusGone)
		return
	}
	ip := clientIP(r)
	if s.limiter.blocked(ip) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	attrs, err := parseDeviceAttrs(body)
	if err != nil {
		s.limiter.fail(ip)
		slog.Warn("enrol parse", "err", err)
		http.Error(w, "could not read device info", http.StatusBadRequest)
		return
	}
	udid, ok := normalizeUDID(attrs.UDID)
	if !ok {
		http.Error(w, "invalid udid", http.StatusBadRequest)
		return
	}
	if _, err := addOrUpdateDevice(s.db, &Device{
		ID: store.ShortID(), BundleID: bundle, UDID: udid,
		Name: strings.TrimSpace(attrs.DeviceName), Product: attrs.Product, Source: "capture",
	}); err != nil {
		slog.Error("enrol add device", "err", err)
	}
	slog.Info("device enrolled", "bundle", bundle, "product", attrs.Product)
	http.Redirect(w, r, "/register/"+token+"/done", http.StatusFound)
}

// handleRegisterSubmit takes a typed UDID from the public page's manual form —
// the fallback when a visitor already knows their identifier.
func (s *Server) handleRegisterSubmit(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	bundle, err := registrationBundle(s.db, token)
	if err != nil || bundle == "" {
		s.renderRegGone(w)
		return
	}
	ip := clientIP(r)
	if s.limiter.blocked(ip) {
		http.Redirect(w, r, "/register/"+token+"?error="+url.QueryEscape("Too many attempts. Wait a minute."), http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	udid, ok := normalizeUDID(r.FormValue("udid"))
	if !ok {
		s.limiter.fail(ip)
		http.Redirect(w, r, "/register/"+token+"?error="+url.QueryEscape("That doesn't look like a UDID."), http.StatusSeeOther)
		return
	}
	if _, err := addOrUpdateDevice(s.db, &Device{
		ID: store.ShortID(), BundleID: bundle, UDID: udid,
		Name: strings.TrimSpace(r.FormValue("name")), Source: "capture",
	}); err != nil {
		slog.Error("submit device", "err", err)
	}
	http.Redirect(w, r, "/register/"+token+"/done", http.StatusSeeOther)
}

func (s *Server) handleRegisterDone(w http.ResponseWriter, r *http.Request) {
	bundle, ok := s.regBundle(w, r)
	if !ok {
		return
	}
	s.rd.Render(w, "register_done.html", map[string]any{"AppName": s.appName(bundle)})
}

// handleAppDelete removes an entire app — every version, its tokens, its
// rich-page metadata, and all of their files.
func (s *Server) handleAppDelete(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	cids, shots, n, err := deleteApp(s.db, bundle)
	if err != nil {
		slog.Error("delete app", "bundle", bundle, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.removeAppFiles(cids, shots)
	slog.Info("app deleted", "bundle", bundle, "versions", n)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// removeAppFiles drops a deleted app's .ipa blobs and screenshot images.
func (s *Server) removeAppFiles(buildCIDs []string, shots []Screenshot) {
	for _, c := range buildCIDs {
		if err := s.blobs.remove(c, ".ipa"); err != nil {
			slog.Warn("could not remove blob", "cid", c, "err", err)
		}
	}
	for _, sh := range shots {
		if err := s.blobs.remove(sh.CID, sh.Ext); err != nil {
			slog.Warn("could not remove screenshot", "cid", sh.CID, "err", err)
		}
	}
}

func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	b, err := getBuild(s.db, id)
	if err != nil {
		slog.Error("get build", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	tok, err := selfToken(s.db, b.ID)
	if err != nil {
		slog.Error("self token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	pageURL := s.publicURL + "/b/" + b.ID
	qrSVG, _, err := EncodeSVG([]byte(pageURL), ECMedium)
	if err != nil {
		slog.Warn("qr encode", "err", err)
		qrSVG = ""
	}
	s.rd.Render(w, "build.html", map[string]any{
		"Build":      b,
		"Expiry":     expiryView(b),
		"InstallURL": s.installLink(tok.Token),
		"PageURL":    pageURL,
		"QR":         template.HTML(qrSVG),
	})
}

// handleDelete removes one version. It returns to the app's version list when
// other versions remain, else to the index.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := getBuild(s.db, id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	dest := "/"
	if b != nil {
		if _, err := deleteBuild(s.db, id); err != nil {
			slog.Error("delete build", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := s.blobs.remove(b.CID, ".ipa"); err != nil {
			slog.Warn("could not remove blob", "cid", b.CID, "err", err)
		}
		if remaining, err := listBuildsByBundle(s.db, b.BundleID); err == nil && len(remaining) > 0 {
			dest = "/app/" + b.BundleID
		}
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// ── shares ───────────────────────────────────────────────────────────────────

func (s *Server) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := getBuild(s.db, id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	ttl := parseTTL(r.FormValue("ttl"))
	max := parseMaxInstalls(r.FormValue("max"))
	label := strings.TrimSpace(r.FormValue("label"))

	tok, err := createShare(s.db, b.ID, ttl, max, label)
	if err != nil {
		slog.Error("create share", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.rd.Render(w, "created.html", map[string]any{
		"Build":     b,
		"ShareURL":  s.publicURL + "/s/" + tok.Token,
		"ExpiresAt": tok.ExpiresAt,
		"Max":       max,
		"Label":     label,
	})
}

func (s *Server) handleShares(w http.ResponseWriter, r *http.Request) {
	shares, err := listShares(s.db)
	if err != nil {
		slog.Error("list shares", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Compute an effective live/dead label for the table.
	type row struct {
		shareRow
		Live bool
		URL  string
	}
	rows := make([]row, 0, len(shares))
	for _, sh := range shares {
		rows = append(rows, row{shareRow: sh, Live: sh.canStart(), URL: s.publicURL + "/s/" + sh.Token.Token})
	}
	s.rd.Render(w, "shares.html", map[string]any{"Shares": rows})
}

func (s *Server) handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	if _, err := revokeToken(s.db, r.PathValue("token")); err != nil {
		slog.Error("revoke token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/shares", http.StatusSeeOther)
}

// handleShareLanding is the public share page — no session. It warns up front
// that only enrolled devices can install, then offers the one-tap link.
func (s *Server) handleShareLanding(w http.ResponseWriter, r *http.Request) {
	tok, code := s.loadToken(r)
	if code != http.StatusOK || tok.Kind != kindShare || !tok.canStart() {
		status := http.StatusGone
		if code == http.StatusNotFound || tok == nil || tok.Kind != kindShare {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		s.rd.Render(w, "gone.html", nil)
		return
	}
	b, err := getBuild(s.db, tok.BuildID)
	if err != nil || b == nil {
		w.WriteHeader(http.StatusGone)
		s.rd.Render(w, "gone.html", nil)
		return
	}
	// Rich content is best-effort on a public page — never fail the install over it.
	meta, shots, err := s.loadAppContent(b.BundleID)
	if err != nil {
		slog.Warn("share content", "err", err)
	}
	s.rd.Render(w, "share.html", map[string]any{
		"Build":       b,
		"Expiry":      expiryView(b),
		"InstallURL":  s.installLink(tok.Token),
		"Label":       tok.Label,
		"Meta":        meta,
		"Screenshots": shots,
	})
}

// ── install session (token-gated, no cookie) ─────────────────────────────────

// loadToken resolves a path token with per-IP enumeration rate-limiting.
// Returns the token, or nil with a status the caller should respond.
func (s *Server) loadToken(r *http.Request) (*Token, int) {
	raw := r.PathValue("token")
	if !tokenPattern.MatchString(raw) {
		return nil, http.StatusNotFound
	}
	ip := clientIP(r)
	if s.limiter.blocked(ip) {
		return nil, http.StatusTooManyRequests
	}
	t, err := getToken(s.db, raw)
	if err != nil {
		slog.Error("get token", "err", err)
		return nil, http.StatusInternalServerError
	}
	if t == nil {
		s.limiter.fail(ip)
		return nil, http.StatusNotFound
	}
	return t, http.StatusOK
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	tok, code := s.loadToken(r)
	if code != http.StatusOK || !tok.canStart() {
		goneText(w)
		return
	}
	b, err := getBuild(s.db, tok.BuildID)
	if err != nil || b == nil {
		goneText(w)
		return
	}
	base := s.publicURL + "/i/" + tok.Token
	xml, err := buildManifest(b, manifestURLs{
		IPA:     base + "/app.ipa",
		Display: base + "/display.png",
		Full:    base + "/full.png",
	})
	if err != nil {
		slog.Error("build manifest", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(xml)
}

func (s *Server) handleIPA(w http.ResponseWriter, r *http.Request) {
	tok, code := s.loadToken(r)
	if code != http.StatusOK || !tok.canServeBytes() {
		goneText(w)
		return
	}
	b, err := getBuild(s.db, tok.BuildID)
	if err != nil || b == nil {
		goneText(w)
		return
	}
	f, size, err := s.blobs.open(b.CID, ".ipa")
	if err != nil {
		slog.Error("open blob", "cid", b.CID, "err", err)
		goneText(w)
		return
	}
	defer f.Close()

	name := b.Filename
	if name == "" {
		name = "app.ipa"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(name)+`"`)

	wasStartable := tok.canStart()
	deliversLast := deliversFinalByte(r.Header.Get("Range"), size)

	var modtime time.Time
	if t, perr := time.Parse(time.RFC3339, b.CreatedAt); perr == nil {
		modtime = t
	}
	http.ServeContent(w, r, name, modtime, f)

	// Count the install only when this response delivered the archive's final
	// byte and the token was still startable when it began — so a multi-range
	// download counts once, on the request that finishes it.
	if r.Method == http.MethodGet && wasStartable && deliversLast {
		if err := recordInstall(s.db, tok, r.UserAgent(), clientIP(r)); err != nil {
			slog.Warn("record install", "err", err)
		} else {
			slog.Info("install delivered", "build", b.ID, "kind", tok.Kind,
				"used", tok.UsedInstalls, "state", tok.State)
		}
	}
}

// handleIcon serves a generated identicon for the install prompt at the given
// pixel size.
func (s *Server) handleIcon(size int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, code := s.loadToken(r)
		if code != http.StatusOK || !tok.canServeBytes() {
			goneText(w)
			return
		}
		b, err := getBuild(s.db, tok.BuildID)
		if err != nil || b == nil {
			goneText(w)
			return
		}
		png, err := iconPNG(b.BundleID, size)
		if err != nil {
			slog.Error("icon", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(png)
	}
}

// ── terminal API (read/delete/share) ─────────────────────────────────────────

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	builds, err := listBuilds(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list builds")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"builds": builds})
}

func (s *Server) handleAPIDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		web.WriteError(w, http.StatusNotFound, "build not found")
		return
	}
	b, err := getBuild(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if b == nil {
		web.WriteError(w, http.StatusNotFound, "build not found")
		return
	}
	if _, err := deleteBuild(s.db, id); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete build")
		return
	}
	if err := s.blobs.remove(b.CID, ".ipa"); err != nil {
		slog.Warn("could not remove blob", "cid", b.CID, "err", err)
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleAPIAppDelete removes an entire app (every version) by bundle id.
func (s *Server) handleAPIAppDelete(w http.ResponseWriter, r *http.Request) {
	bundle := r.PathValue("bundle")
	cids, shots, n, err := deleteApp(s.db, bundle)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not delete app")
		return
	}
	if n == 0 {
		web.WriteError(w, http.StatusNotFound, "app not found")
		return
	}
	s.removeAppFiles(cids, shots)
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": bundle, "versions": n})
}

func (s *Server) handleAPIShare(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := getBuild(s.db, id)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if b == nil {
		web.WriteError(w, http.StatusNotFound, "build not found")
		return
	}
	q := r.URL.Query()
	ttl := parseTTL(q.Get("ttl"))
	max := parseMaxInstalls(q.Get("max"))
	tok, err := createShare(s.db, b.ID, ttl, max, strings.TrimSpace(q.Get("label")))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not create share")
		return
	}
	web.WriteJSON(w, http.StatusCreated, map[string]any{
		"token":       tok.Token,
		"shareURL":    s.publicURL + "/s/" + tok.Token,
		"expiresAt":   tok.ExpiresAt,
		"maxInstalls": max,
	})
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countBuilds(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "sideload",
		"ok":      true,
		"builds":  n,
	})
}

// ── helpers ──────────────────────────────────────────────────────────────────

// installLink builds the itms-services OTA install URL for a token. It is
// returned as template.URL because html/template otherwise filters the
// non-allowlisted itms-services: scheme to a dead "#ZgotmplZ".
func (s *Server) installLink(token string) template.URL {
	manifest := s.publicURL + "/i/" + token + "/manifest.plist"
	return template.URL("itms-services://?action=download-manifest&url=" + url.QueryEscape(manifest))
}

func goneText(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusGone)
	_, _ = io.WriteString(w, "This install link is no longer valid.\n")
}

// deliversFinalByte reports whether serving the given Range (empty = full GET)
// delivers the last byte of a size-byte file — the signal that an install
// download has completed.
func deliversFinalByte(rangeHeader string, size int64) bool {
	if rangeHeader == "" {
		return true
	}
	const p = "bytes="
	if !strings.HasPrefix(rangeHeader, p) {
		return false
	}
	for _, spec := range strings.Split(rangeHeader[len(p):], ",") {
		spec = strings.TrimSpace(spec)
		dash := strings.IndexByte(spec, '-')
		if dash < 0 {
			continue
		}
		startStr := strings.TrimSpace(spec[:dash])
		endStr := strings.TrimSpace(spec[dash+1:])
		switch {
		case startStr == "": // bytes=-N suffix → last N bytes
			if n, err := strconv.ParseInt(endStr, 10, 64); err == nil && n > 0 {
				return true
			}
		case endStr == "": // bytes=N- open-ended → through the end
			return true
		default: // bytes=N-M
			if end, err := strconv.ParseInt(endStr, 10, 64); err == nil && end >= size-1 {
				return true
			}
		}
	}
	return false
}

// parseTTL maps a share TTL choice to a duration; unknown values fall back to
// the 30-minute default.
func parseTTL(choice string) time.Duration {
	switch choice {
	case "2h":
		return 2 * time.Hour
	case "24h":
		return 24 * time.Hour
	case "30m", "":
		return 30 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// parseMaxInstalls maps a max-installs choice to a count; 0 means unlimited.
// The default is single-use.
func parseMaxInstalls(choice string) int {
	switch choice {
	case "unlimited", "0":
		return 0
	case "3":
		return 3
	case "1", "":
		return 1
	default:
		if n, err := strconv.Atoi(choice); err == nil && n >= 0 {
			return n
		}
		return 1
	}
}

// sanitizeFilename reduces an upload name to a safe base for Content-Disposition.
func sanitizeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == "/" {
		return "app.ipa"
	}
	name = strings.NewReplacer(`"`, "", "\\", "", "\n", "", "\r", "").Replace(name)
	if !strings.HasSuffix(strings.ToLower(name), ".ipa") {
		name += ".ipa"
	}
	return name
}

// ExpiryView is the provisioning-profile expiry summary the install pages show.
type ExpiryView struct {
	Known   bool
	Date    string
	Days    int
	Expired bool
	Warn    bool // under the warning threshold
}

const expiryWarnDays = 14

func expiryView(b *Build) ExpiryView {
	if b.ProfileExpiry == "" {
		return ExpiryView{}
	}
	t, err := time.Parse(time.RFC3339, b.ProfileExpiry)
	if err != nil {
		return ExpiryView{}
	}
	days := int(time.Until(t).Hours() / 24)
	expired := !t.After(time.Now())
	return ExpiryView{
		Known:   true,
		Date:    t.Format("2006-01-02"),
		Days:    days,
		Expired: expired,
		Warn:    !expired && days < expiryWarnDays,
	}
}

// relAge renders an RFC 3339 timestamp as compact relative age.
func relAge(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// sizeText renders a byte count for the meta line.
func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
