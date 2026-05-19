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
	"os"
	"os/signal"
	"path"
	"strconv"
	"syscall"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
)

//go:embed templates
var assets embed.FS

// pageSize is how many days (NASA) or items (UFO) one archive page shows.
const pageSize = 14

// publicMaxAge is the Cache-Control lifetime on the public read endpoints — a
// calendar day. A past day's photo never changes; the current day settles
// within the day, so a day of caching is safe and lets a CDN absorb load.
const publicMaxAge = 86400

// Server holds the running calendar service.
type Server struct {
	db        *sql.DB
	fetcher   *fetcher
	templates map[string]*template.Template
	assetVer  string // content hash of the stylesheet — cache-busts the URL
}

// backfillCommand warms the NASA cache from the configured calendar start
// through the requested end date. It never walks before Jan 1 2026.
func backfillCommand(start, end string) error {
	if !validDate(start) || !validDate(end) {
		return fmt.Errorf("dates must be YYYY-MM-DD")
	}
	if start < calendarStart {
		start = calendarStart
	}
	today := todayUTC()
	if end > today {
		end = today
	}
	if end < start {
		return fmt.Errorf("empty range after clamping to %s..%s", calendarStart, today)
	}
	db, err := openDB(store.Env("CALENDAR_DB_PATH", "calendar.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	s := &Server{db: db, fetcher: newFetcher(store.Env("NASA_API_KEY", "DEMO_KEY"))}
	if err := s.nasaEnsureRange(start, end); err != nil {
		return err
	}
	slog.Info("backfill complete", "source", sourceNASA, "start", start, "end", end)
	return nil
}

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("CALENDAR_DB_PATH", "calendar.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	tmpl, err := parseTemplates()
	if err != nil {
		return err
	}

	s := &Server{
		db:        db,
		fetcher:   newFetcher(store.Env("NASA_API_KEY", "DEMO_KEY")),
		templates: tmpl,
		assetVer:  cid.Of([]byte(theme.CSS))[:16],
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

	// HTML pages — public, no auth: the calendar is a read-only viewer.
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /day/{date}", s.handleDay)
	mux.HandleFunc("GET /archive", s.handleArchive)

	// Public JSON API — the photo/photos/sources reads are cacheable for a day.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/sources", s.handleAPISources)
	mux.HandleFunc("GET /api/photo", s.handleAPIToday)
	mux.HandleFunc("GET /api/photo/{date}", s.handleAPIDay)
	mux.HandleFunc("GET /api/photos", s.handleAPIPhotos)

	// Shared theme stylesheet.
	mux.HandleFunc("GET /static/styles.css", handleCSS)

	return cors(logRequests(mux))
}

// ── archive paging ─────────────────────────────────────────────────────────

// archiveResult is one page of an archive listing plus its paging metadata.
type archiveResult struct {
	Photos  []Photo
	Page    int
	Pages   int
	Total   int
	HasPrev bool
	HasNext bool
}

// archive returns one page of photos for a source, newest first. For NASA it
// warms a real calendar range in a single upstream call, then reads it from
// cache; for UFO it pages the synthetic-dated scrape. Page 1 is the newest.
func (s *Server) archive(source string, page int) (archiveResult, error) {
	if page < 1 {
		page = 1
	}
	if source == sourceUFO {
		if err := s.ufoEnsure(); err != nil {
			slog.Warn("ufo ensure failed", "err", err)
		}
		total, err := countPhotos(s.db, sourceUFO)
		if err != nil {
			return archiveResult{}, err
		}
		photos, err := listPhotos(s.db, sourceUFO, pageSize, (page-1)*pageSize)
		if err != nil {
			return archiveResult{}, err
		}
		pages := pageCount(total)
		return archiveResult{
			Photos: photos, Page: page, Pages: pages, Total: total,
			HasPrev: page > 1, HasNext: page < pages,
		}, nil
	}

	// NASA: page 1 ends today; each older page steps back pageSize days. Farfield
	// intentionally starts at calendarStart rather than APOD's 1995 archive.
	today, _ := time.Parse(dateLayout, todayUTC())
	epoch, _ := time.Parse(dateLayout, calendarStart)
	end := today.AddDate(0, 0, -(page-1)*pageSize)
	start := end.AddDate(0, 0, -(pageSize - 1))
	if start.Before(epoch) {
		start = epoch
	}
	// Warm the real calendar range for the requested page, but paginate over the
	// cache that actually exists. NASA can rate-limit DEMO_KEY/backfills; if the
	// cache is only partially warm, the archive must not advertise empty pages for
	// days we do not have yet.
	if !end.Before(epoch) {
		startS, endS := start.Format(dateLayout), end.Format(dateLayout)
		if err := s.nasaEnsureRange(startS, endS); err != nil {
			return archiveResult{}, err
		}
	}
	total, err := countPhotos(s.db, sourceNASA)
	if err != nil {
		return archiveResult{}, err
	}
	photos, err := listPhotos(s.db, sourceNASA, pageSize, (page-1)*pageSize)
	if err != nil {
		return archiveResult{}, err
	}
	pages := pageCount(total)
	return archiveResult{
		Photos: photos, Page: page, Pages: pages, Total: total,
		HasPrev: page > 1, HasNext: page < pages,
	}, nil
}

// pageCount returns how many pages total items span, at least one.
func pageCount(total int) int {
	if total <= 0 {
		return 1
	}
	return (total + pageSize - 1) / pageSize
}

// ── HTML handlers ──────────────────────────────────────────────────────────

// handleIndex renders the current day's photo for the selected source.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	source := canonicalSource(r.URL.Query().Get("source"))
	photo, date, err := s.todayPhoto(source)
	if err != nil {
		s.fail(w, "today photo", err)
		return
	}
	s.renderPhoto(w, source, photo, date, true)
}

// handleDay renders one specific day's photo.
func (s *Server) handleDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if !validDate(date) {
		http.NotFound(w, r)
		return
	}
	source := canonicalSource(r.URL.Query().Get("source"))
	photo, err := s.photoForDate(source, date)
	if err != nil {
		s.fail(w, "day photo", err)
		return
	}
	s.renderPhoto(w, source, photo, date, false)
}

// renderPhoto renders the photo page for one (source, date). isToday suppresses
// the "next" step, since nothing is newer than the current day.
func (s *Server) renderPhoto(w http.ResponseWriter, source string, photo *Photo, date string, isToday bool) {
	prev, err := neighborDate(s.db, source, date, true)
	if err != nil {
		s.fail(w, "prev date", err)
		return
	}
	next := ""
	if !isToday {
		if next, err = neighborDate(s.db, source, date, false); err != nil {
			s.fail(w, "next date", err)
			return
		}
	}
	info := sourceInfo(source)
	sq := sourceQuery(source)
	jsonURL := "/api/photo" + sq
	if !isToday {
		jsonURL = "/api/photo/" + date + sq
	}
	s.render(w, "photo.html", map[string]any{
		"Photo":       photo,
		"Date":        date,
		"SourceLabel": info.Label,
		"IsUFO":       source == sourceUFO,
		"HomeURL":     "/" + sq,
		"ArchiveURL":  "/archive" + sq,
		"JSONURL":     jsonURL,
		"SwitchURL":   "/" + sourceQuery(otherSource(source)),
		"PrevURL":     dayURL(prev, source),
		"NextURL":     dayURL(next, source),
	})
}

// handleArchive renders a paginated grid of previous days/items.
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	source := canonicalSource(r.URL.Query().Get("source"))
	res, err := s.archive(source, pageParam(r))
	if err != nil {
		s.fail(w, "archive", err)
		return
	}
	unit := "days"
	if source == sourceUFO {
		unit = "items"
	}
	prevURL, nextURL := "", ""
	if res.HasPrev {
		prevURL = archiveURL(source, res.Page-1)
	}
	if res.HasNext {
		nextURL = archiveURL(source, res.Page+1)
	}
	sq := sourceQuery(source)
	s.render(w, "archive.html", map[string]any{
		"Photos":      res.Photos,
		"Page":        res.Page,
		"Pages":       res.Pages,
		"Total":       res.Total,
		"Unit":        unit,
		"SourceLabel": sourceInfo(source).Label,
		"SourceQuery": sq,
		"IsUFO":       source == sourceUFO,
		"HomeURL":     "/" + sq,
		"JSONURL":     "/api/photos" + sq,
		"SwitchURL":   "/" + sourceQuery(otherSource(source)),
		"PrevURL":     prevURL,
		"NextURL":     nextURL,
	})
}

// ── JSON API handlers ──────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	nasa, err := countPhotos(s.db, sourceNASA)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read index")
		return
	}
	ufo, err := countPhotos(s.db, sourceUFO)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read index")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "calendar", "ok": true, "nasa": nasa, "ufo": ufo,
	})
}

func (s *Server) handleAPISources(w http.ResponseWriter, r *http.Request) {
	cacheable(w)
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

func (s *Server) handleAPIToday(w http.ResponseWriter, r *http.Request) {
	source := canonicalSource(r.URL.Query().Get("source"))
	photo, _, err := s.todayPhoto(source)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load photo")
		return
	}
	s.writePhoto(w, r, source, photo)
}

func (s *Server) handleAPIDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if !validDate(date) {
		writeError(w, http.StatusBadRequest, "malformed date — expected YYYY-MM-DD")
		return
	}
	source := canonicalSource(r.URL.Query().Get("source"))
	photo, err := s.photoForDate(source, date)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load photo")
		return
	}
	s.writePhoto(w, r, source, photo)
}

func (s *Server) handleAPIPhotos(w http.ResponseWriter, r *http.Request) {
	source := canonicalSource(r.URL.Query().Get("source"))
	res, err := s.archive(source, pageParam(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list photos")
		return
	}
	cacheable(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"source": source,
		"page":   res.Page,
		"pages":  res.Pages,
		"total":  res.Total,
		"photos": res.Photos,
	})
}

// writePhoto emits one photo as JSON, cacheable for a day. When the photo
// exists its content CID is sent as a strong ETag, so a client holding the
// current version gets a 304.
func (s *Server) writePhoto(w http.ResponseWriter, r *http.Request, source string, photo *Photo) {
	cacheable(w)
	if photo == nil {
		writeError(w, http.StatusNotFound, "no photo for that date")
		return
	}
	prev, _ := neighborDate(s.db, source, photo.Date, true)
	next, _ := neighborDate(s.db, source, photo.Date, false)
	etag := `"` + photo.CID + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": source,
		"photo":  photo,
		"prev":   prev,
		"next":   next,
	})
}

// ── helpers ────────────────────────────────────────────────────────────────

// pageParam reads the ?page= query value, defaulting to page 1.
func pageParam(r *http.Request) int {
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
		return p
	}
	return 1
}

// sourceQuery returns the query string that selects a source — empty for the
// default NASA source, "?source=ufo" for the UFO source.
func sourceQuery(source string) string {
	if source == sourceUFO {
		return "?source=ufo"
	}
	return ""
}

// dayURL builds a /day link for a date, or "" when there is no such date.
func dayURL(date, source string) string {
	if date == "" {
		return ""
	}
	return "/day/" + date + sourceQuery(source)
}

// archiveURL builds an /archive link for a page, carrying the source.
func archiveURL(source string, page int) string {
	u := "/archive?page=" + strconv.Itoa(page)
	if source == sourceUFO {
		u += "&source=ufo"
	}
	return u
}

// cacheable marks a response publicly cacheable for publicMaxAge seconds.
func cacheable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", publicMaxAge))
}

func handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(w, theme.CSS)
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
		t, err := template.New("base.html").ParseFS(assets, "templates/base.html", page)
		if err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
}

// render writes a page through base.html, buffering first so a template error
// never produces a half-written response.
func (s *Server) render(w http.ResponseWriter, page string, data map[string]any) {
	t, ok := s.templates[page]
	if !ok {
		slog.Error("unknown template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data["AssetVer"] = s.assetVer
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		slog.Error("render failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// fail logs an internal error and returns a 500.
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

// cors adds permissive CORS headers so a browser on another origin (the
// website) can read the public API, and answers preflight requests.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, If-None-Match")
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
