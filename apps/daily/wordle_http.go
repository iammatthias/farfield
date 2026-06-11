package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/web"
)

// HTTP surface of the wordle artifact. Everything is public and stateless.
// The guess endpoint is a server call because the answer is secret — a client
// cannot score itself. The answer appears in a response exactly once: when a
// game is over (solved, or six misses).

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

// renderWordlePage renders the game page for one date — an empty grid for
// everyone, playable statelessly in-page. In-progress guesses live in the
// browser's localStorage, restored by wordle.js, so the HTML is the same for
// every visitor and publicly cacheable.
func (s *Server) renderWordlePage(w http.ResponseWriter, r *http.Request, date string) {
	if !wordleDayValid(date) {
		http.NotFound(w, r)
		return
	}

	rows := make([][]wordleTile, wordleGuesses)
	for i := range rows {
		rows[i] = make([]wordleTile, 5)
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

	cacheFor(w, todayMaxAge)
	s.rd.Render(w, "wordle.html", map[string]any{
		"Date":    date,
		"CID":     wordleCID(date),
		"Rows":    rows,
		"Epoch":   artifactEpoch,
		"JSONURL": jsonURL,
		"PrevURL": prevURL,
		"NextURL": nextURL,
		"JSVer":   wordleJSVer,
		"Nav":     navData(date, "wordle"),
	})
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
