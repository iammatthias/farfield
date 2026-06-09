package main

import (
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

// pageSize is how many days one archive page shows.
const pageSize = 14

// publicMaxAge is the Cache-Control lifetime on the public read endpoints — a
// calendar day. A past day's photo never changes; the current day settles
// within the day, so a day of caching is safe and lets a CDN absorb load.
const publicMaxAge = 86400

// Server holds the running calendar service.
type Server struct {
	db      *sql.DB
	fetcher *fetcher
	rd      *web.Renderer
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
	slog.Info("backfill complete", "start", start, "end", end)
	return nil
}

// backfillOnStartup warms the APOD archive across the full calendar range so a
// freshly deployed instance is populated without waiting for the first
// visitor. NASA needs a real NASA_API_KEY to fill; with DEMO_KEY the range call
// rate-limits and the cache fills lazily as pages are viewed instead.
func (s *Server) backfillOnStartup() {
	if err := s.nasaEnsureRange(calendarStart, todayUTC()); err != nil {
		slog.Warn("startup nasa backfill failed", "err", err)
	}
}

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("CALENDAR_DB_PATH", "calendar.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	tmpl, err := web.ParseTemplates(assets, calendarFuncs)
	if err != nil {
		return err
	}

	s := &Server{
		db:      db,
		fetcher: newFetcher(store.Env("NASA_API_KEY", "DEMO_KEY")),
		rd:      &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
	}

	// Backfill the whole calendar — Jan 1 through today — in the background so
	// a fresh deploy is populated without blocking startup.
	go s.backfillOnStartup()

	return web.Serve(host, port, s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// HTML pages — public, no auth: the calendar is a read-only viewer.
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /day/{date}", s.handleDay)
	mux.HandleFunc("GET /archive", s.handleArchive)

	// Public JSON API — the photo and photos reads are cacheable for a day.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/photo", s.handleAPIToday)
	mux.HandleFunc("GET /api/photo/{date}", s.handleAPIDay)
	mux.HandleFunc("GET /api/photos", s.handleAPIPhotos)

	// Shared theme stylesheet.
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// Everything the calendar serves itself is text — HTML, JSON; the photos
	// are hot-linked from NASA — so Gzip wraps the whole mux. The default CORS
	// method list (GET, OPTIONS) matches this read-only API.
	return web.CORS(web.LogRequests(web.Gzip(mux)))
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

// archive returns one page of photos, newest first. Page 1 ends today; each
// older page steps back pageSize days. It warms the real calendar range for
// the requested page in one upstream call, then paginates over the cache.
func (s *Server) archive(page int) (archiveResult, error) {
	if page < 1 {
		page = 1
	}
	// Farfield intentionally starts at calendarStart, not APOD's 1995 archive.
	today, _ := time.Parse(dateLayout, todayUTC())
	epoch, _ := time.Parse(dateLayout, calendarStart)
	end := today.AddDate(0, 0, -(page-1)*pageSize)
	start := end.AddDate(0, 0, -(pageSize - 1))
	if start.Before(epoch) {
		start = epoch
	}
	// NASA can rate-limit DEMO_KEY / backfills; if the cache is only partially
	// warm, the archive must not advertise empty pages for days we lack.
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

// handleIndex renders the current day's photo.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	photo, date, err := s.todayPhoto()
	if err != nil {
		s.fail(w, "today photo", err)
		return
	}
	s.renderPhoto(w, photo, date, true)
}

// handleDay renders one specific day's photo.
func (s *Server) handleDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if !validDate(date) {
		http.NotFound(w, r)
		return
	}
	photo, err := s.photoForDate(date)
	if err != nil {
		s.fail(w, "day photo", err)
		return
	}
	s.renderPhoto(w, photo, date, false)
}

// renderPhoto renders the photo page for one date. isToday suppresses the
// "next" step, since nothing is newer than the current day.
func (s *Server) renderPhoto(w http.ResponseWriter, photo *Photo, date string, isToday bool) {
	prev, err := neighborDate(s.db, sourceNASA, date, true)
	if err != nil {
		s.fail(w, "prev date", err)
		return
	}
	next := ""
	if !isToday {
		if next, err = neighborDate(s.db, sourceNASA, date, false); err != nil {
			s.fail(w, "next date", err)
			return
		}
	}
	jsonURL := "/api/photo"
	if !isToday {
		jsonURL = "/api/photo/" + date
	}
	s.rd.Render(w, "photo.html", map[string]any{
		"Photo":      photo,
		"Date":       date,
		"ArchiveURL": "/archive",
		"JSONURL":    jsonURL,
		"PrevURL":    dayURL(prev),
		"NextURL":    dayURL(next),
	})
}

// handleArchive renders a paginated grid of previous days.
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	res, err := s.archive(pageParam(r))
	if err != nil {
		s.fail(w, "archive", err)
		return
	}
	prevURL, nextURL := "", ""
	if res.HasPrev {
		prevURL = archiveURL(res.Page - 1)
	}
	if res.HasNext {
		nextURL = archiveURL(res.Page + 1)
	}
	s.rd.Render(w, "archive.html", map[string]any{
		"Photos":  res.Photos,
		"Page":    res.Page,
		"Pages":   res.Pages,
		"Total":   res.Total,
		"JSONURL": "/api/photos",
		"PrevURL": prevURL,
		"NextURL": nextURL,
	})
}

// ── JSON API handlers ──────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	nasa, err := countPhotos(s.db, sourceNASA)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not read index")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"service": "calendar", "ok": true, "nasa": nasa,
	})
}

func (s *Server) handleAPIToday(w http.ResponseWriter, r *http.Request) {
	photo, _, err := s.todayPhoto()
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not load photo")
		return
	}
	s.writePhoto(w, r, photo)
}

func (s *Server) handleAPIDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if !validDate(date) {
		web.WriteError(w, http.StatusBadRequest, "malformed date — expected YYYY-MM-DD")
		return
	}
	photo, err := s.photoForDate(date)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not load photo")
		return
	}
	s.writePhoto(w, r, photo)
}

func (s *Server) handleAPIPhotos(w http.ResponseWriter, r *http.Request) {
	res, err := s.archive(pageParam(r))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list photos")
		return
	}
	cacheable(w)
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"source": sourceNASA,
		"page":   res.Page,
		"pages":  res.Pages,
		"total":  res.Total,
		"photos": res.Photos,
	})
}

// writePhoto emits one photo as JSON, cacheable for a day. When the photo
// exists its content CID is sent as a strong ETag, so a client holding the
// current version gets a 304.
func (s *Server) writePhoto(w http.ResponseWriter, r *http.Request, photo *Photo) {
	cacheable(w)
	if photo == nil {
		web.WriteError(w, http.StatusNotFound, "no photo for that date")
		return
	}
	prev, _ := neighborDate(s.db, sourceNASA, photo.Date, true)
	next, _ := neighborDate(s.db, sourceNASA, photo.Date, false)
	w.Header().Set("ETag", `"`+photo.CID+`"`)
	if web.ETagMatch(r, photo.CID) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"source": sourceNASA,
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

// dayURL builds a /day link for a date, or "" when there is no such date.
func dayURL(date string) string {
	if date == "" {
		return ""
	}
	return "/day/" + date
}

// archiveURL builds an /archive link for a page.
func archiveURL(page int) string {
	return "/archive?page=" + strconv.Itoa(page)
}

// cacheable marks a response publicly cacheable for publicMaxAge seconds.
func cacheable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", publicMaxAge))
}

// calendarFuncs are the template helpers. mediaKind lets the photo template
// branch on whether a video URL is a directly-playable file versus an embed.
var calendarFuncs = template.FuncMap{"mediaKind": mediaKind}

// fail logs an internal error and returns a 500.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
