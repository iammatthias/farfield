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
	if err := s.parseTemplates(); err != nil {
		return err
	}

	go s.sweepLoop()

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
	mux.HandleFunc("GET /b/{id}", s.auth.RequireSession(s.handleBuild))
	mux.HandleFunc("POST /b/{id}/share", s.auth.RequireSession(s.handleShareCreate))
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

	// Public share landing.
	mux.HandleFunc("GET /s/{token}", s.handleShareLanding)

	// Agent API — X-API-Key.
	mux.HandleFunc("POST /api/builds", s.auth.RequireAPIKey(s.handleAPIUpload))
	mux.HandleFunc("GET /api/builds", s.auth.RequireAPIKey(s.handleAPIList))
	mux.HandleFunc("DELETE /api/builds/{id}", s.auth.RequireAPIKey(s.handleAPIDelete))
	mux.HandleFunc("POST /api/builds/{id}/share", s.auth.RequireAPIKey(s.handleAPIShare))

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// Gzip self-skips octet-stream and Range, so wrapping the whole mux leaves
	// .ipa byte serving untouched while compressing HTML/JSON. Logging sits
	// outside for the final status; pulse innermost so timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(pulse.Middleware(s.db, "sideload")(mux))),
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
func (s *Server) ingest(r *http.Request, src io.Reader, filename, gitCommit, notes string) (*Build, error) {
	fullCID, size, err := s.blobs.spool(src, maxIPABytes)
	if err != nil {
		return nil, err
	}
	id := fullCID[:16]

	meta, err := parseIPA(s.blobs.path(fullCID))
	if err != nil {
		// Not a valid .ipa. Drop the bytes unless a prior build already claims
		// this content address.
		if existing, gerr := getBuild(s.db, id); gerr == nil && existing == nil {
			_ = s.blobs.remove(fullCID)
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
	b, err := s.ingest(r, src, filename, r.FormValue("commit"), r.FormValue("notes"))
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
	b, err := s.ingest(r, src, filename, q.Get("commit"), q.Get("notes"))
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
	s.rd.Render(w, "app.html", map[string]any{
		"BundleID": bundle,
		"AppName":  builds[0].AppName,
		"Builds":   builds,
	})
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

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := getBuild(s.db, id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if b != nil {
		if _, err := deleteBuild(s.db, id); err != nil {
			slog.Error("delete build", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := s.blobs.remove(b.CID); err != nil {
			slog.Warn("could not remove blob", "cid", b.CID, "err", err)
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
	s.rd.Render(w, "share.html", map[string]any{
		"Build":      b,
		"Expiry":     expiryView(b),
		"InstallURL": s.installLink(tok.Token),
		"Label":      tok.Label,
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
	f, size, err := s.blobs.open(b.CID)
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
	if err := s.blobs.remove(b.CID); err != nil {
		slog.Warn("could not remove blob", "cid", b.CID, "err", err)
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"deleted": id})
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
