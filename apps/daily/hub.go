package main

import (
	"net/http"
)

// The hub — the daily index at / and /{date}. One page, four cards: the
// day's photo, art plate, sudoku, and wordle, each linking into its
// artifact. The hub reads only what already exists (the photos cache, the
// solve-state table, pure derivations); it never triggers an upstream fetch.

// hubDayValid reports whether a date has a hub page: well-formed, on or
// after the artifact epoch, not in the future.
func hubDayValid(date string) bool {
	return validDate(date) && date >= artifactEpoch && date <= todayUTC()
}

// handleHubToday renders today's hub.
func (s *Server) handleHubToday(w http.ResponseWriter, r *http.Request) {
	s.renderHubPage(w, r, todayUTC())
}

// handleHubDay renders one date's hub. The GET /{date} pattern sits at the
// same depth as the literal artifact routes (/photo, /art, …); Go's ServeMux
// prefers literal segments over wildcards, so those never land here — only
// genuinely unclaimed single-segment paths do, and non-dates 404.
func (s *Server) handleHubDay(w http.ResponseWriter, r *http.Request) {
	s.renderHubPage(w, r, r.PathValue("date"))
}

// hubGameStatus is one game card's play-state line.
type hubGameStatus struct {
	Status string // "", or "solved …" / progress text
	Solved bool
	Streak int
}

// renderHubPage renders the hub for one date. Solve-state lines appear only
// for authed visitors, so an authed page is never publicly cacheable; the
// anonymous page caches like the photo index.
func (s *Server) renderHubPage(w http.ResponseWriter, r *http.Request, date string) {
	if !hubDayValid(date) {
		http.NotFound(w, r)
		return
	}
	authed := s.authed(r)
	today := todayUTC()
	isToday := date == today

	// Photo card — cache-only read; days the cache lacks render a placeholder.
	photo, err := getPhoto(s.db, sourceNASA, date)
	if err != nil {
		s.fail(w, "hub photo", err)
		return
	}

	// Art card — the plate is an <img> on the already-cached SVG route.
	artURL, artSVG := "", ""
	if _, ok := artDayIndex(date); ok {
		artURL = "/art/" + date
		artSVG = "/art/" + date + ".svg"
	}

	// Sudoku and wordle cards — difficulty plus, when authed, play state.
	p := sudokuFor(date)
	sudoku, err := s.hubGameStatus(domainSudoku, date, today, authed)
	if err != nil {
		s.fail(w, "hub sudoku", err)
		return
	}
	wordle, err := s.hubGameStatus(domainWordle, date, today, authed)
	if err != nil {
		s.fail(w, "hub wordle", err)
		return
	}

	prevURL, nextURL := "", ""
	if date > artifactEpoch {
		prevURL = "/" + addDays(date, -1)
	}
	if !isToday {
		if n := addDays(date, 1); n == today {
			nextURL = "/"
		} else {
			nextURL = "/" + n
		}
	}

	if authed {
		w.Header().Set("Cache-Control", "private, no-cache")
	} else {
		cacheFor(w, todayMaxAge)
	}
	s.rd.Render(w, "hub.html", map[string]any{
		"Date":       date,
		"Weekday":    p.Weekday,
		"IsToday":    isToday,
		"Authed":     authed,
		"Photo":      photo,
		"ArtURL":     artURL,
		"ArtSVG":     artSVG,
		"Difficulty": p.Difficulty,
		"ClueCount":  p.ClueCount,
		"Sudoku":     sudoku,
		"Wordle":     wordle,
		"Epoch":      artifactEpoch,
		"PrevURL":    prevURL,
		"NextURL":    nextURL,
		"Nav":        navData(date),
	})
}

// hubGameStatus assembles one game card's line: streak always (it is the
// instance's progress either way), play state only when authed.
func (s *Server) hubGameStatus(domain, date, today string, authed bool) (hubGameStatus, error) {
	streak, err := solveStreak(s.db, domain, today)
	if err != nil {
		return hubGameStatus{}, err
	}
	gs := hubGameStatus{Streak: streak}
	if !authed {
		return gs, nil
	}
	st, err := getSolveState(s.db, domain, date)
	if err != nil {
		return hubGameStatus{}, err
	}
	switch {
	case st == nil:
		gs.Status = "unplayed"
	case st.Solved:
		gs.Solved = true
		gs.Status = "solved"
		if st.SolveMs > 0 {
			gs.Status += " · " + fmtSolveTime(st.SolveMs)
		}
	default:
		gs.Status = "in progress"
	}
	return gs, nil
}

// navData builds the cross-artifact nav strip links for one date — today
// uses the canonical undated routes, past days the date-addressed ones.
func navData(date string) map[string]string {
	link := func(root string) string {
		if date == todayUTC() {
			return root
		}
		return root + "/" + date
	}
	hub := "/"
	if date != todayUTC() {
		hub = "/" + date
	}
	return map[string]string{
		"Hub":    hub,
		"Photo":  link("/photo"),
		"Art":    link("/art"),
		"Sudoku": link("/sudoku"),
		"Wordle": link("/wordle"),
	}
}
