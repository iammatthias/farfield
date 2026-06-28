package main

import (
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/iammatthias/farfield/lib/backup"
	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

// Server holds the running backup admin service.
type Server struct {
	db   *sql.DB
	auth *web.Auth
	rd   *web.Renderer
	// snapMu single-flights snapshot runs: the scheduler tick and the admin
	// button can never run concurrently against the same registry.
	snapMu sync.Mutex
	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder
}

func run(host, port string) error {
	db, err := openDB(store.Env("BACKUP_DB_PATH", "data/backup.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	tmpl, err := web.ParseTemplates(assets, tmplFuncs)
	if err != nil {
		return err
	}

	s := &Server{
		db: db,
		auth: &web.Auth{
			DB:           db,
			Password:     store.Env("PASSWORD", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd: &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
	}

	// In-process scheduler: snapshots no longer depend on a host cron
	// existing. BACKUP_INTERVAL accepts any Go duration; "0" disables.
	if interval := store.Env("BACKUP_INTERVAL", "6h"); interval != "0" {
		d, err := time.ParseDuration(interval)
		if err != nil {
			return fmt.Errorf("BACKUP_INTERVAL %q: %w", interval, err)
		}
		go s.snapshotLoop(d)
	} else {
		slog.Info("snapshot scheduler disabled (BACKUP_INTERVAL=0)")
	}

	s.pulse = pulse.New(s.db, "backup")
	defer s.pulse.Close()
	return web.Serve(host, port, s.routes())
}

// snapshotLoop snapshots every app shortly after boot (deploys restart the
// stack, so this doubles as a post-deploy backup), then on every interval.
func (s *Server) snapshotLoop(interval time.Duration) {
	slog.Info("snapshot scheduler running", "interval", interval)
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	for {
		<-timer.C
		s.runScheduledSnapshot()
		timer.Reset(interval)
	}
}

func (s *Server) runScheduledSnapshot() {
	if !s.snapMu.TryLock() {
		slog.Info("snapshot already running — scheduler tick skipped")
		return
	}
	defer s.snapMu.Unlock()
	for _, res := range snapshotAll(s.db) {
		switch {
		case res.Err != "":
			slog.Error("scheduled snapshot failed", "app", res.App, "err", res.Err)
		case res.Skipped:
			slog.Info("scheduled snapshot unchanged", "app", res.App, "cid", res.CID)
		default:
			slog.Info("scheduled snapshot", "app", res.App, "cid", res.CID, "size", res.Size)
		}
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// HTML admin UI — session-gated.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleIndex))
	mux.HandleFunc("POST /snapshot", s.auth.RequireSession(s.handleSnapshot))
	mux.HandleFunc("POST /backups/{id}/delete", s.auth.RequireSession(s.handleDelete))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// Everything backup serves is text — HTML, JSON — so Gzip wraps the
	// whole mux. Logging sits outside so the recorded status is the final one;
	// pulse traffic recording sits innermost so logged timings stay real.
	return web.LogRequests(web.Gzip(s.pulse.Wrap(mux)))
}

// ── handlers ───────────────────────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	backups, err := listBackups(s.db)
	if err != nil {
		s.fail(w, "list backups", err)
		return
	}
	s.rd.Render(w, "index.html", map[string]any{
		"Backups": backups,
		"Targets": targets(),
		"Notice":  r.URL.Query().Get("msg"),
	})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.snapMu.TryLock() {
		http.Redirect(w, r, "/?msg="+url.QueryEscape("A snapshot is already running."),
			http.StatusSeeOther)
		return
	}
	defer s.snapMu.Unlock()
	fresh, unchanged, failed := 0, 0, 0
	for _, res := range snapshotAll(s.db) {
		switch {
		case res.Err != "":
			slog.Error("snapshot failed", "app", res.App, "err", res.Err)
			failed++
		case res.Skipped:
			unchanged++
		default:
			fresh++
		}
	}
	msg := fmt.Sprintf("%d new, %d unchanged", fresh, unchanged)
	if failed > 0 {
		msg += fmt.Sprintf(", %d failed (see logs)", failed)
	}
	http.Redirect(w, r, "/?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, err := getBackup(s.db, id)
	if err != nil {
		s.fail(w, "get backup", err)
		return
	}
	if b != nil {
		if _, err := deleteBackup(s.db, id); err != nil {
			s.fail(w, "delete backup", err)
			return
		}
		// Drop the snapshot bytes from blobs too — but only once no other
		// record still points at that (content-addressed) CID. If the
		// reference check fails, keep the bytes: deleting a snapshot another
		// record still points at would destroy a restorable backup.
		used, err := cidReferenced(s.db, b.CID)
		if err != nil {
			slog.Warn("could not check snapshot references; keeping blob", "cid", b.CID, "err", err)
		} else if !used {
			if err := backup.Delete(blobsURL(), blobsKey(), b.CID); err != nil {
				slog.Warn("could not delete snapshot from blobs", "cid", b.CID, "err", err)
			}
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	n, err := countBackups(s.db)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "backup", "ok": true, "backups": n,
	})
}

// ── login ──────────────────────────────────────────────────────────────────

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.rd.Render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

// ── helpers ────────────────────────────────────────────────────────────────

func blobsURL() string { return store.Env("BLOBS_URL", "http://127.0.0.1:8789") }
func blobsKey() string { return store.Env("BLOBS_API_KEY", "") }

var tmplFuncs = template.FuncMap{"humanSize": humanSize}

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

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
