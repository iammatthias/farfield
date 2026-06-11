package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/web"
)

// HTTP surface of the sudoku artifact. Reads are public — the puzzle derives
// from the date and anyone may play in-page. Only the solve-state write is
// session-gated, and the solution itself never leaves the server: posted
// entries are validated against a freshly re-derived solution.

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
	Entry    string // restored entry, "" when blank
	BR, BB   bool
}

// renderSudokuPage renders the puzzle page for one date. When the visitor is
// authed, saved entries are restored into the grid. The page varies on the
// session cookie, so it is never publicly cacheable.
func (s *Server) renderSudokuPage(w http.ResponseWriter, r *http.Request, date string) {
	if !sudokuDayValid(date) {
		http.NotFound(w, r)
		return
	}
	p := sudokuFor(date)
	authed := s.authed(r)

	var st *solveState
	if authed {
		var err error
		if st, err = getSolveState(s.db, domainSudoku, date); err != nil {
			s.fail(w, "sudoku state", err)
			return
		}
	}
	entries := ""
	if st != nil {
		entries = st.Payload
	}

	cells := make([]sudokuCell, 81)
	for i := range cells {
		c := sudokuCell{
			Index: i, Row: i/9 + 1, Col: i%9 + 1,
			BR: i%9 == 2 || i%9 == 5, BB: i/9 == 2 || i/9 == 5,
		}
		if d := p.Clues[i]; d != '0' {
			c.Given, c.Val = true, string(d)
		} else if len(entries) == 81 && entries[i] != '0' && entries[i] >= '1' && entries[i] <= '9' {
			c.Entry = string(entries[i])
		}
		cells[i] = c
	}

	streak, err := solveStreak(s.db, domainSudoku, todayUTC())
	if err != nil {
		s.fail(w, "sudoku streak", err)
		return
	}

	status := ""
	solveMs := int64(0)
	switch {
	case st != nil && st.Solved:
		solveMs = st.SolveMs
		status = "solved in " + fmtSolveTime(st.SolveMs)
	case st != nil:
		solveMs = st.SolveMs
		status = "saved"
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

	// The grid carries restored per-session state — never share a cached copy.
	w.Header().Set("Cache-Control", "private, no-cache")
	s.rd.Render(w, "sudoku.html", map[string]any{
		"Date":       p.Date,
		"Clues":      p.Clues,
		"ClueCount":  p.ClueCount,
		"Difficulty": p.Difficulty,
		"Weekday":    p.Weekday,
		"CID":        p.CID,
		"Cells":      cells,
		"Authed":     authed,
		"Solved":     st != nil && st.Solved,
		"SolveMs":    solveMs,
		"Status":     status,
		"Streak":     streak,
		"Epoch":      artifactEpoch,
		"JSONURL":    jsonURL,
		"PrevURL":    prevURL,
		"NextURL":    nextURL,
		"JSVer":      sudokuJSVer,
		"Nav":        navData(date),
	})
}

// fmtSolveTime renders a solve duration as m:ss.
func fmtSolveTime(ms int64) string {
	sec := ms / 1000
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
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

// ── solve-state write ──────────────────────────────────────────────────────

// sudokuStateBody is the POST /sudoku/{date}/state request body.
type sudokuStateBody struct {
	Entries string `json:"entries"` // 81 chars, '0' for blank
	SolveMs int64  `json:"solveMs"`
}

// handleSudokuState persists posted progress for one date — partial saves
// just persist; a complete grid is judged against the re-derived solution.
// Conflicts are reported only for a complete-but-wrong grid: the indices
// whose entries differ from the solution.
func (s *Server) handleSudokuState(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if !sudokuDayValid(date) {
		web.WriteError(w, http.StatusNotFound, "no sudoku for that date")
		return
	}
	var body sudokuStateBody
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
	if body.SolveMs < 0 {
		body.SolveMs = 0
	}
	if err := upsertSolveState(s.db, &solveState{
		Domain:  domainSudoku,
		Date:    date,
		Payload: body.Entries,
		Solved:  solved,
		SolveMs: body.SolveMs,
	}); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not save state")
		return
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
