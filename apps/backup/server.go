package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
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
	"github.com/iammatthias/farfield/lib/backup"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
)

//go:embed templates
var assets embed.FS

// Server holds the running backup admin service.
type Server struct {
	db           *sql.DB
	templates    map[string]*template.Template
	password     string
	cookieSecure bool
}

func run(host, port string) error {
	db, err := openDB(store.Env("BACKUP_DB_PATH", "data/backup.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	tmpl, err := parseTemplates()
	if err != nil {
		return err
	}

	s := &Server{
		db:           db,
		templates:    tmpl,
		password:     store.Env("PASSWORD", ""),
		cookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
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
	mux.HandleFunc("POST /snapshot", s.requireSession(s.handleSnapshot))
	mux.HandleFunc("POST /backups/{id}/delete", s.requireSession(s.handleDelete))

	// Login — public HTML.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /logout", s.handleLogout)

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /static/styles.css", handleCSS)

	return logRequests(mux)
}

// ── handlers ───────────────────────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	backups, err := listBackups(s.db)
	if err != nil {
		s.fail(w, "list backups", err)
		return
	}
	s.render(w, "index.html", map[string]any{
		"Backups": backups,
		"Targets": targets(),
		"Notice":  r.URL.Query().Get("msg"),
	})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
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
		// record still points at that (content-addressed) CID.
		if used, _ := cidReferenced(s.db, b.CID); !used {
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
		writeError(w, http.StatusInternalServerError, "could not read database")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "backup", "ok": true, "backups": n,
	})
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

// ── helpers ────────────────────────────────────────────────────────────────

func blobsURL() string { return store.Env("BLOBS_URL", "http://127.0.0.1:8789") }
func blobsKey() string { return store.Env("BLOBS_API_KEY", "") }

func handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, theme.CSS)
}

var tmplFuncs = template.FuncMap{"humanSize": humanSize}

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

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
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
