package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/iammatthias/farfield/lib/pulse"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

//go:embed templates
var assets embed.FS

// pageSize is how many days one archive page shows.
const pageSize = 14

// publicMaxAge is the Cache-Control lifetime on date-addressed reads — a
// calendar day. A past day's photo never changes, so a day of caching is safe
// and lets a CDN absorb load.
const publicMaxAge = 86400

// todayMaxAge is the Cache-Control lifetime on the rolling views — the index,
// the undated /api/photo, and the archive. They change when a new day's photo
// posts, so they get minutes of caching, not a day.
const todayMaxAge = 600

// Server holds the running daily service.
type Server struct {
	db      *sql.DB
	fetcher *fetcher
	rd      *web.Renderer
}

// backfillCommand warms the NASA cache from the configured photo start
// through the requested end date. It never walks before Jan 1 2026.
func backfillCommand(start, end string) error {
	if !validDate(start) || !validDate(end) {
		return fmt.Errorf("dates must be YYYY-MM-DD")
	}
	if start < photoStart {
		start = photoStart
	}
	today := todayUTC()
	if end > today {
		end = today
	}
	if end < start {
		return fmt.Errorf("empty range after clamping to %s..%s", photoStart, today)
	}
	db, err := openDB(store.Env("DAILY_DB_PATH", "daily.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	s := &Server{db: db, fetcher: newFetcher(store.Env("NASA_API_KEY", "DEMO_KEY"))}
	if err := s.nasaEnsureRange(context.Background(), start, end); err != nil {
		return err
	}
	slog.Info("backfill complete", "start", start, "end", end)
	return nil
}

// backfillOnStartup warms the APOD archive across the full photo range so a
// freshly deployed instance is populated without waiting for the first
// visitor. NASA needs a real NASA_API_KEY to fill; with DEMO_KEY the range call
// rate-limits and the cache fills lazily as pages are viewed instead.
func (s *Server) backfillOnStartup() {
	if err := s.nasaEnsureRange(context.Background(), photoStart, todayUTC()); err != nil {
		slog.Warn("startup nasa backfill failed", "err", err)
	}
}

// run wires up the service and serves until interrupted.
func run(host, port string) error {
	db, err := openDB(store.Env("DAILY_DB_PATH", "daily.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	tmpl, err := web.ParseTemplates(assets, templateFuncs)
	if err != nil {
		return err
	}

	s := &Server{
		db:      db,
		fetcher: newFetcher(store.Env("NASA_API_KEY", "DEMO_KEY")),
		rd:      &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
	}

	// Backfill the whole photo archive — Jan 1 through today — in the background so
	// a fresh deploy is populated without blocking startup.
	go s.backfillOnStartup()

	return web.Serve(host, port, s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// HTML pages — public, no auth: the photo artifact is a read-only viewer.
	mux.HandleFunc("GET /photo", s.handleIndex)
	mux.HandleFunc("GET /photo/archive", s.handleArchive)
	mux.HandleFunc("GET /photo/{date}", s.handleDay)

	// The hub — today's four artifacts on one page, and the same view for
	// any past date. The /{date} wildcard sits at the same depth as the
	// literal artifact routes; ServeMux prefers literal segments, so /photo,
	// /art, /sudoku, /wordle, /static/…, /api/… all win over it
	// (TestHubRouting pins this down). Non-date strays 404 in the handler.
	mux.HandleFunc("GET /{$}", s.handleHubToday)
	mux.HandleFunc("GET /{date}", s.handleHubDay)

	// Legacy paths from when the photo was the whole app. They redirect
	// permanently to the /photo artifact.
	mux.HandleFunc("GET /archive", redirect("/photo/archive"))
	mux.HandleFunc("GET /day/{date}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/photo/"+r.PathValue("date"), http.StatusMovedPermanently)
	})

	// The art artifact — a deterministic generative terrain accreting one
	// cell per day along a 4-D Hilbert curve. Everything derives from the
	// date; nothing is stored. /art/{date} also serves /art/{date}.svg.
	mux.HandleFunc("GET /art", s.handleArtToday)
	mux.HandleFunc("GET /art.svg", s.handleArtSVGToday)
	mux.HandleFunc("GET /art/structure", s.handleArtStructure)
	mux.HandleFunc("GET /art/{date}", s.handleArtDay)

	// The sudoku artifact — a deterministic daily puzzle derived from the
	// date. Everything is public and stateless: the check endpoint judges a
	// posted grid against a freshly re-derived solution and persists nothing.
	mux.HandleFunc("GET /sudoku", s.handleSudokuToday)
	mux.HandleFunc("GET /sudoku/{date}", s.handleSudokuDay)
	mux.HandleFunc("POST /sudoku/{date}/check", s.handleSudokuCheck)

	// The wordle artifact — one secret five-letter word a day, derived from
	// the date. Everything is public and stateless; guess feedback is a
	// server call because the answer is secret, so a client cannot score
	// itself.
	mux.HandleFunc("GET /wordle", s.handleWordleToday)
	mux.HandleFunc("GET /wordle/{date}", s.handleWordleDay)
	mux.HandleFunc("POST /wordle/{date}/guess", s.handleWordleGuess)

	// Public JSON API — the photo and photos reads are cacheable for a day.
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/photo", s.handleAPIToday)
	mux.HandleFunc("GET /api/photo/{date}", s.handleAPIDay)
	mux.HandleFunc("GET /api/photos", s.handleAPIPhotos)
	mux.HandleFunc("GET /api/art", s.handleAPIArtToday)
	// The literal /structure segment wins over the {date} wildcard.
	mux.HandleFunc("GET /api/art/structure", s.handleAPIArtStructure)
	mux.HandleFunc("GET /api/art/{date}", s.handleAPIArtDay)
	mux.HandleFunc("GET /api/sudoku", s.handleAPISudokuToday)
	mux.HandleFunc("GET /api/sudoku/{date}", s.handleAPISudokuDay)
	mux.HandleFunc("GET /api/wordle", s.handleAPIWordleToday)
	mux.HandleFunc("GET /api/wordle/{date}", s.handleAPIWordleDay)

	// Shared theme stylesheet, plus the app-local game grid scripts and the
	// structure viewer (scene script + vendored three.js, all immutable).
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())
	mux.HandleFunc("GET /static/sudoku.js", sudokuJSHandler())
	mux.HandleFunc("GET /static/wordle.js", wordleJSHandler())
	mux.HandleFunc("GET /static/structure.js", immutableJSHandler(structureJS, structureJSVer))
	mux.HandleFunc("GET /static/vendor/three.module.min.js", immutableJSHandler(threeJS, threeJSVer))
	mux.HandleFunc("GET /static/vendor/OrbitControls.js", immutableJSHandler(orbitControlsJS, orbitJSVer))

	// Everything the service serves itself is text — HTML, JSON; the photos
	// are hot-linked from NASA — so Gzip wraps the whole mux. The default CORS
	// method list (GET, OPTIONS) matches this read-only API. Pulse traffic
	// recording sits innermost so logged timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(pulse.Middleware(s.db, "daily")(mux))))
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
// older page steps back pageSize days. It warms the real date range for
// the requested page in one upstream call, then paginates over the cache.
func (s *Server) archive(ctx context.Context, page int) (archiveResult, error) {
	if page < 1 {
		page = 1
	}
	// Farfield intentionally starts at photoStart, not APOD's 1995 archive.
	today, _ := time.Parse(dateLayout, todayUTC())
	epoch, _ := time.Parse(dateLayout, photoStart)
	end := today.AddDate(0, 0, -(page-1)*pageSize)
	start := end.AddDate(0, 0, -(pageSize - 1))
	if start.Before(epoch) {
		start = epoch
	}
	// NASA can rate-limit DEMO_KEY / backfills; if the cache is only partially
	// warm, the archive must not advertise empty pages for days we lack.
	if !end.Before(epoch) {
		startS, endS := start.Format(dateLayout), end.Format(dateLayout)
		if err := s.nasaEnsureRange(ctx, startS, endS); err != nil {
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
	photo, date, err := s.todayPhoto(r.Context())
	if err != nil {
		s.fail(w, "today photo", err)
		return
	}
	s.renderPhoto(w, r, photo, date, true)
}

// handleDay renders one specific day's photo.
func (s *Server) handleDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if !validDate(date) {
		http.NotFound(w, r)
		return
	}
	photo, err := s.photoForDate(r.Context(), date)
	if err != nil {
		s.fail(w, "day photo", err)
		return
	}
	s.renderPhoto(w, r, photo, date, false)
}

// renderPhoto renders the photo page for one date. isToday suppresses the
// "next" step, since nothing is newer than the current day. Pages carry no
// per-visitor state, so the HTML is publicly cacheable — a past day's page
// for a day, today's (which rolls forward) for minutes.
func (s *Server) renderPhoto(w http.ResponseWriter, r *http.Request, photo *Photo, date string, isToday bool) {
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
	maxAge := todayMaxAge
	if !isToday {
		maxAge = publicMaxAge // a past day's photo page never changes
	}
	cacheFor(w, maxAge)
	s.rd.Render(w, "photo.html", map[string]any{
		"Photo":      photo,
		"Date":       date,
		"ArchiveURL": "/photo/archive",
		"JSONURL":    jsonURL,
		"PrevURL":    dayURL(prev),
		"NextURL":    dayURL(next),
		"Nav":        navData(date, "photo"),
	})
}

// handleArchive renders a paginated grid of previous days. It rolls forward
// when a new day posts, so it caches for minutes like the other rolling views.
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	res, err := s.archive(r.Context(), pageParam(r))
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
	cacheFor(w, todayMaxAge)
	s.rd.Render(w, "archive.html", map[string]any{
		"Photos":  res.Photos,
		"Page":    res.Page,
		"Pages":   res.Pages,
		"Total":   res.Total,
		"JSONURL": "/api/photos",
		"PrevURL": prevURL,
		"NextURL": nextURL,
		"Nav":     navData(todayUTC(), "photo"),
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
		"service": "daily", "ok": true, "nasa": nasa,
	})
}

func (s *Server) handleAPIToday(w http.ResponseWriter, r *http.Request) {
	photo, _, err := s.todayPhoto(r.Context())
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not load photo")
		return
	}
	// The undated photo rolls forward when a new day posts — short cache only.
	s.writePhoto(w, r, photo, todayMaxAge)
}

func (s *Server) handleAPIDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if !validDate(date) {
		web.WriteError(w, http.StatusBadRequest, "malformed date — expected YYYY-MM-DD")
		return
	}
	photo, err := s.photoForDate(r.Context(), date)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not load photo")
		return
	}
	s.writePhoto(w, r, photo, publicMaxAge)
}

func (s *Server) handleAPIPhotos(w http.ResponseWriter, r *http.Request) {
	res, err := s.archive(r.Context(), pageParam(r))
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not list photos")
		return
	}
	cacheFor(w, publicMaxAge)
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"source": sourceNASA,
		"page":   res.Page,
		"pages":  res.Pages,
		"total":  res.Total,
		"photos": res.Photos,
	})
}

// writePhoto emits one photo as JSON, cacheable for maxAge seconds. When the
// photo exists its content CID is sent as a strong ETag, so a client holding
// the current version gets a 304.
func (s *Server) writePhoto(w http.ResponseWriter, r *http.Request, photo *Photo, maxAge int) {
	if photo == nil {
		web.WriteError(w, http.StatusNotFound, "no photo for that date")
		return
	}
	cacheFor(w, maxAge)
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

// dayURL builds a /photo link for a date, or "" when there is no such date.
func dayURL(date string) string {
	if date == "" {
		return ""
	}
	return "/photo/" + date
}

// archiveURL builds a /photo/archive link for a page.
func archiveURL(page int) string {
	return "/photo/archive?page=" + strconv.Itoa(page)
}

// redirect permanently redirects a legacy path to target, carrying the query
// string (e.g. /archive?page=2 → /photo/archive?page=2) along.
func redirect(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dest := target
		if q := r.URL.RawQuery; q != "" {
			dest += "?" + q
		}
		http.Redirect(w, r, dest, http.StatusMovedPermanently)
	}
}

// cacheFor marks a response publicly cacheable for maxAge seconds. With no
// sessions anywhere, nothing daily serves varies per visitor, so the HTML
// pages share it with the SVG plates and the JSON API.
func cacheFor(w http.ResponseWriter, maxAge int) {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
}

// templateFuncs are the template helpers. mediaKind lets the photo template
// branch on whether a video URL is a directly-playable file versus an embed.
var templateFuncs = template.FuncMap{"mediaKind": mediaKind}

// fail logs an internal error and returns a 500.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
