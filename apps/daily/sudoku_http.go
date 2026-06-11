package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/web"
)

// HTTP surface of the sudoku artifact. Everything is public — the puzzle
// derives from the date and anyone may play in-page. The check endpoint is
// stateless, and the solution itself never leaves the server: posted entries
// are validated against a freshly re-derived solution.

// sudokuJS is the app-local grid script. It is fingerprinted like the shared
// theme assets — the page links /static/sudoku.js?v={{.JSVer}} and the
// handler serves it immutable, so an edited script changes its URL.
//
//go:embed static/sudoku.js
var sudokuJS []byte

var sudokuJSVer = cid.Of(sudokuJS)[:16]

// sudokuJSHandler serves the grid script with immutable caching, mirroring
// the theme asset handler. Gzip comes from the app-wide middleware.
func sudokuJSHandler() http.HandlerFunc {
	etag := sudokuJSVer
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/javascript; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("ETag", `"`+etag+`"`)
		if web.ETagMatch(r, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(sudokuJS)
	}
}

// ── HTML handlers ──────────────────────────────────────────────────────────

// handleSudokuToday renders today's puzzle page.
func (s *Server) handleSudokuToday(w http.ResponseWriter, r *http.Request) {
	s.renderSudokuPage(w, r, todayUTC())
}

// handleSudokuDay renders one date's puzzle page.
func (s *Server) handleSudokuDay(w http.ResponseWriter, r *http.Request) {
	s.renderSudokuPage(w, r, r.PathValue("date"))
}

// sudokuCell is one rendered grid cell. BR/BB mark the heavy 3×3 box rules
// after columns/rows 3 and 6; Row/Col are 1-based for accessibility labels.
type sudokuCell struct {
	Index    int
	Row, Col int
	Given    bool
	Val      string // the given digit
	BR, BB   bool
}

// renderSudokuPage renders the puzzle page for one date. The page is the same
// for every visitor — in-progress entries live in the browser's localStorage,
// restored by sudoku.js — so the HTML is publicly cacheable.
func (s *Server) renderSudokuPage(w http.ResponseWriter, r *http.Request, date string) {
	if !sudokuDayValid(date) {
		http.NotFound(w, r)
		return
	}
	p := sudokuFor(date)

	cells := make([]sudokuCell, 81)
	for i := range cells {
		c := sudokuCell{
			Index: i, Row: i/9 + 1, Col: i%9 + 1,
			BR: i%9 == 2 || i%9 == 5, BB: i/9 == 2 || i/9 == 5,
		}
		if d := p.Clues[i]; d != '0' {
			c.Given, c.Val = true, string(d)
		}
		cells[i] = c
	}

	isToday := date == todayUTC()
	prevURL, nextURL := "", ""
	if date > artifactEpoch {
		prevURL = "/sudoku/" + addDays(date, -1)
	}
	if !isToday {
		nextURL = "/sudoku/" + addDays(date, 1)
	}
	jsonURL := "/api/sudoku"
	if !isToday {
		jsonURL = "/api/sudoku/" + date
	}

	cacheFor(w, todayMaxAge)
	s.rd.Render(w, "sudoku.html", map[string]any{
		"Date":       p.Date,
		"Clues":      p.Clues,
		"ClueCount":  p.ClueCount,
		"Difficulty": p.Difficulty,
		"Weekday":    p.Weekday,
		"CID":        p.CID,
		"Cells":      cells,
		"Epoch":      artifactEpoch,
		"JSONURL":    jsonURL,
		"PrevURL":    prevURL,
		"NextURL":    nextURL,
		"JSVer":      sudokuJSVer,
		"Nav":        navData(date, "sudoku"),
	})
}

// ── JSON API handlers ──────────────────────────────────────────────────────

func (s *Server) handleAPISudokuToday(w http.ResponseWriter, r *http.Request) {
	s.writeSudokuJSON(w, r, todayUTC(), todayMaxAge)
}

func (s *Server) handleAPISudokuDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	maxAge := publicMaxAge
	if date == todayUTC() {
		maxAge = todayMaxAge
	}
	s.writeSudokuJSON(w, r, date, maxAge)
}

// writeSudokuJSON emits one day's public puzzle — date, difficulty, clues,
// CID — with the CID as ETag. The solution is not in the payload, by design.
func (s *Server) writeSudokuJSON(w http.ResponseWriter, r *http.Request, date string, maxAge int) {
	if !sudokuDayValid(date) {
		web.WriteError(w, http.StatusNotFound, "no sudoku for that date")
		return
	}
	p := sudokuFor(date)
	cacheFor(w, maxAge)
	w.Header().Set("ETag", `"`+p.CID+`"`)
	if web.ETagMatch(r, p.CID) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"date":       p.Date,
		"difficulty": p.Difficulty,
		"clues":      p.Clues,
		"cid":        p.CID,
	})
}

// ── check endpoint ─────────────────────────────────────────────────────────

// sudokuCheckBody is the POST /sudoku/{date}/check request body.
type sudokuCheckBody struct {
	Entries string `json:"entries"` // 81 chars, '0' for blank
}

// handleSudokuCheck judges a posted grid against the re-derived solution —
// public and stateless, nothing is persisted. A partial grid is simply
// unsolved; conflicts are reported only for a complete-but-wrong grid: the
// indices whose entries differ from the solution.
func (s *Server) handleSudokuCheck(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if !sudokuDayValid(date) {
		web.WriteError(w, http.StatusNotFound, "no sudoku for that date")
		return
	}
	var body sudokuCheckBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	p, sol := sudokuDerive(date)
	if !validSudokuEntries(body.Entries, p.Clues) {
		web.WriteError(w, http.StatusBadRequest,
			"entries must be 81 digits ('0' for blank) consistent with the givens")
		return
	}
	solution := gridString(sol)
	solved := body.Entries == solution
	conflicts := []int{}
	if !solved && !strings.ContainsRune(body.Entries, '0') {
		for i := range 81 {
			if body.Entries[i] != solution[i] {
				conflicts = append(conflicts, i)
			}
		}
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"solved":    solved,
		"conflicts": conflicts,
	})
}

// validSudokuEntries reports whether an entries string is well-formed — 81
// digit characters — and consistent with the puzzle's givens.
func validSudokuEntries(entries, clues string) bool {
	if len(entries) != 81 {
		return false
	}
	for i := range 81 {
		if entries[i] < '0' || entries[i] > '9' {
			return false
		}
		if clues[i] != '0' && entries[i] != clues[i] {
			return false
		}
	}
	return true
}
