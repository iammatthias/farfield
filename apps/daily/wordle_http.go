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

// HTTP surface of the wordle artifact. The page and the guess endpoint are
// public — feedback has to be a server call because the answer is secret —
// and only the solve-state write needs a session. The answer appears in a
// response exactly once: when a game is over (solved, or six misses).

// wordleJS is the app-local grid script, fingerprinted like sudoku.js: the
// page links /static/wordle.js?v={{.JSVer}} and the handler serves it
// immutable, so an edited script changes its URL.
//
//go:embed static/wordle.js
var wordleJS []byte

var wordleJSVer = cid.Of(wordleJS)[:16]

// wordleJSHandler serves the grid script with immutable caching.
func wordleJSHandler() http.HandlerFunc {
	etag := wordleJSVer
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/javascript; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("ETag", `"`+etag+`"`)
		if web.ETagMatch(r, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(wordleJS)
	}
}

// wordleStatePayload is what persists in solve_state.payload for one day —
// the guesses and the hard-mode flag; solved and solve_ms live in their own
// columns.
type wordleStatePayload struct {
	Guesses []string `json:"guesses"`
	Hard    bool     `json:"hard"`
}

// ── HTML handlers ──────────────────────────────────────────────────────────

// handleWordleToday renders today's game page.
func (s *Server) handleWordleToday(w http.ResponseWriter, r *http.Request) {
	s.renderWordlePage(w, r, todayUTC())
}

// handleWordleDay renders one date's game page.
func (s *Server) handleWordleDay(w http.ResponseWriter, r *http.Request) {
	s.renderWordlePage(w, r, r.PathValue("date"))
}

// wordleTile is one rendered grid cell. State is "" (empty), "g", "y", or
// "-" — the same letters the guess endpoint speaks.
type wordleTile struct {
	Ch    string
	State string
}

// renderWordlePage renders the game page for one date. Authed visitors get
// their saved guesses restored and re-scored server-side; everyone else gets
// an empty grid playable statelessly in-page. The page varies on the session
// cookie, so it is never publicly cacheable.
func (s *Server) renderWordlePage(w http.ResponseWriter, r *http.Request, date string) {
	if !wordleDayValid(date) {
		http.NotFound(w, r)
		return
	}
	answer := wordleAnswer(date)
	authed := s.authed(r)

	var st *solveState
	if authed {
		var err error
		if st, err = getSolveState(s.db, domainWordle, date); err != nil {
			s.fail(w, "wordle state", err)
			return
		}
	}
	var saved wordleStatePayload
	if st != nil && st.Payload != "" {
		// A malformed payload (it never should be — writes are validated)
		// degrades to an empty game rather than a 500.
		_ = json.Unmarshal([]byte(st.Payload), &saved)
	}
	saved.Guesses = sanitizeWordleGuesses(saved.Guesses, answer)

	solved := false
	rows := make([][]wordleTile, wordleGuesses)
	for i := range rows {
		row := make([]wordleTile, 5)
		if i < len(saved.Guesses) {
			g := saved.Guesses[i]
			fb := wordleFeedback(answer, g)
			for j := range 5 {
				row[j] = wordleTile{Ch: strings.ToUpper(string(g[j])), State: string(fb[j])}
			}
			if g == answer {
				solved = true
			}
		}
		rows[i] = row
	}
	over := solved || len(saved.Guesses) >= wordleGuesses

	streak, err := solveStreak(s.db, domainWordle, todayUTC())
	if err != nil {
		s.fail(w, "wordle streak", err)
		return
	}

	status := ""
	solveMs := int64(0)
	if st != nil {
		solveMs = st.SolveMs
	}
	switch {
	case solved:
		status = fmt.Sprintf("solved in %d/%d", len(saved.Guesses), wordleGuesses)
		if solveMs > 0 {
			status += " · " + fmtSolveTime(solveMs)
		}
	case over:
		status = "out of guesses"
	case len(saved.Guesses) > 0:
		status = fmt.Sprintf("%d/%d guessed", len(saved.Guesses), wordleGuesses)
	}

	guessesJSON, _ := json.Marshal(saved.Guesses)

	// The answer reaches the page only when the game is over, and only on
	// the authed page — the same rule the guess endpoint follows.
	answerOut := ""
	if over && authed {
		answerOut = strings.ToUpper(answer)
	}

	isToday := date == todayUTC()
	prevURL, nextURL := "", ""
	if date > artifactEpoch {
		prevURL = "/wordle/" + addDays(date, -1)
	}
	if !isToday {
		nextURL = "/wordle/" + addDays(date, 1)
	}
	jsonURL := "/api/wordle"
	if !isToday {
		jsonURL = "/api/wordle/" + date
	}

	// The grid carries restored per-session state — never share a cached copy.
	w.Header().Set("Cache-Control", "private, no-cache")
	s.rd.Render(w, "wordle.html", map[string]any{
		"Date":        date,
		"CID":         wordleCID(date),
		"Rows":        rows,
		"Guesses":     len(saved.Guesses),
		"Authed":      authed,
		"Hard":        saved.Hard,
		"Solved":      solved,
		"Over":        over,
		"Answer":      answerOut,
		"Status":      status,
		"SolveMs":     solveMs,
		"GuessesJSON": string(guessesJSON),
		"Streak":      streak,
		"Epoch":       artifactEpoch,
		"JSONURL":     jsonURL,
		"PrevURL":     prevURL,
		"NextURL":     nextURL,
		"JSVer":       wordleJSVer,
		"Nav":         navData(date),
	})
}

// sanitizeWordleGuesses keeps the leading run of playable guesses: well
// formed, in the dictionary (or the answer), at most six, and nothing after
// the answer is hit. State writes enforce the same rule, so this only
// matters for payloads from older code.
func sanitizeWordleGuesses(guesses []string, answer string) []string {
	out := make([]string, 0, wordleGuesses)
	for _, g := range guesses {
		g = strings.ToLower(g)
		if len(out) >= wordleGuesses || !wordleWordWellFormed(g) || !wordleGuessAllowed(g, answer) {
			break
		}
		out = append(out, g)
		if g == answer {
			break
		}
	}
	return out
}

// ── guess endpoint ─────────────────────────────────────────────────────────

// wordleGuessBody is the POST /wordle/{date}/guess request body. Prior
// guesses come from the client because the endpoint is stateless; the server
// re-scores them itself and never trusts client-side colors.
type wordleGuessBody struct {
	Guess string   `json:"guess"`
	Prior []string `json:"prior"`
	Hard  bool     `json:"hard"`
}

// handleWordleGuess scores one guess. An out-of-dictionary or hard-mode-
// violating guess is rejected with invalid=true and no feedback — it
// consumes nothing. The answer appears in the response only when this guess
// ends the game.
func (s *Server) handleWordleGuess(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if !wordleDayValid(date) {
		web.WriteError(w, http.StatusNotFound, "no wordle for that date")
		return
	}
	var body wordleGuessBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	answer := wordleAnswer(date)

	// Prior guesses must themselves be a playable, unfinished game.
	prior := make([]string, 0, len(body.Prior))
	for _, p := range body.Prior {
		p = strings.ToLower(p)
		if !wordleWordWellFormed(p) || !wordleGuessAllowed(p, answer) {
			web.WriteError(w, http.StatusBadRequest, "prior guesses are not playable words")
			return
		}
		prior = append(prior, p)
	}
	if len(prior) >= wordleGuesses {
		web.WriteError(w, http.StatusBadRequest, "game is already over")
		return
	}
	for _, p := range prior {
		if p == answer {
			web.WriteError(w, http.StatusBadRequest, "game is already over")
			return
		}
	}

	guess := strings.ToLower(strings.TrimSpace(body.Guess))
	if !wordleWordWellFormed(guess) || !wordleGuessAllowed(guess, answer) {
		web.WriteJSON(w, http.StatusOK, map[string]any{
			"invalid": true, "reason": "not in word list",
		})
		return
	}
	if body.Hard {
		if v := wordleHardViolation(answer, prior, guess); v != "" {
			web.WriteJSON(w, http.StatusOK, map[string]any{
				"invalid": true, "reason": v,
			})
			return
		}
	}

	fb := wordleFeedback(answer, guess)
	feedback := make([]string, 5)
	for i := range 5 {
		feedback[i] = string(fb[i])
	}
	solved := guess == answer
	over := solved || len(prior)+1 >= wordleGuesses
	resp := map[string]any{
		"invalid":  false,
		"feedback": feedback,
		"solved":   solved,
		"over":     over,
	}
	if over {
		resp["answer"] = answer
	}
	web.WriteJSON(w, http.StatusOK, resp)
}

// ── solve-state write ──────────────────────────────────────────────────────

// wordleStateBody is the POST /wordle/{date}/state request body.
type wordleStateBody struct {
	Guesses []string `json:"guesses"`
	SolveMs int64    `json:"solveMs"`
	Hard    bool     `json:"hard"`
}

// handleWordleState persists posted progress for one date. Guesses are
// validated server-side — playable words, at most six, none after the
// answer — and solved is recomputed, never trusted from the client.
func (s *Server) handleWordleState(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if !wordleDayValid(date) {
		web.WriteError(w, http.StatusNotFound, "no wordle for that date")
		return
	}
	var body wordleStateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		web.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	answer := wordleAnswer(date)
	if len(body.Guesses) > wordleGuesses {
		web.WriteError(w, http.StatusBadRequest, "too many guesses")
		return
	}
	guesses := make([]string, 0, len(body.Guesses))
	solved := false
	for i, g := range body.Guesses {
		g = strings.ToLower(g)
		if !wordleWordWellFormed(g) || !wordleGuessAllowed(g, answer) {
			web.WriteError(w, http.StatusBadRequest, "guesses must be playable five-letter words")
			return
		}
		if solved {
			web.WriteError(w, http.StatusBadRequest, "no guesses after the answer")
			return
		}
		if g == answer && i == len(body.Guesses)-1 {
			solved = true
		} else if g == answer {
			web.WriteError(w, http.StatusBadRequest, "no guesses after the answer")
			return
		}
		guesses = append(guesses, g)
	}
	if body.SolveMs < 0 {
		body.SolveMs = 0
	}
	payload, err := json.Marshal(wordleStatePayload{Guesses: guesses, Hard: body.Hard})
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not encode state")
		return
	}
	if err := upsertSolveState(s.db, &solveState{
		Domain:  domainWordle,
		Date:    date,
		Payload: string(payload),
		Solved:  solved,
		SolveMs: body.SolveMs,
	}); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not save state")
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"solved": solved,
		"over":   solved || len(guesses) >= wordleGuesses,
	})
}

// ── JSON API handlers ──────────────────────────────────────────────────────

func (s *Server) handleAPIWordleToday(w http.ResponseWriter, r *http.Request) {
	s.writeWordleJSON(w, r, todayUTC(), todayMaxAge)
}

func (s *Server) handleAPIWordleDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	maxAge := publicMaxAge
	if date == todayUTC() {
		maxAge = todayMaxAge
	}
	s.writeWordleJSON(w, r, date, maxAge)
}

// writeWordleJSON emits one day's public record: date and CID, nothing else.
// The CID hashes the answer (see wordleCID), so it identifies the day's word
// without revealing it — the answer itself is never in public JSON.
func (s *Server) writeWordleJSON(w http.ResponseWriter, r *http.Request, date string, maxAge int) {
	if !wordleDayValid(date) {
		web.WriteError(w, http.StatusNotFound, "no wordle for that date")
		return
	}
	c := wordleCID(date)
	cacheFor(w, maxAge)
	w.Header().Set("ETag", `"`+c+`"`)
	if web.ETagMatch(r, c) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{"date": date, "cid": c})
}
