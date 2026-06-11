package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iammatthias/farfield/lib/cid"
)

func TestWordleListsWellFormed(t *testing.T) {
	if n := len(wordleAnswers); n < 2000 {
		t.Fatalf("answers list has %d words, want >= 2000", n)
	}
	if n := len(wordleValid); n < 10000 {
		t.Fatalf("validity list has %d words, want >= 10000", n)
	}
	for _, w := range wordleAnswers {
		if !wordleWordWellFormed(w) {
			t.Fatalf("answer %q is not five lowercase letters", w)
		}
		if !wordleValid[w] {
			t.Fatalf("answer %q is missing from the validity dictionary", w)
		}
	}
}

func TestWordleAnswerDeterministicAndDaily(t *testing.T) {
	if a, b := wordleAnswer("2026-06-03"), wordleAnswer("2026-06-03"); a != b {
		t.Errorf("same date must derive the same answer: %q vs %q", a, b)
	}
	// A run of consecutive days never repeats a word (the shuffled list only
	// wraps after ~6.3 years) and never walks alphabetical neighbors.
	seen := map[string]bool{}
	for d := "2026-05-01"; d <= "2026-05-31"; d = addDays(d, 1) {
		w := wordleAnswer(d)
		if !wordleWordWellFormed(w) {
			t.Fatalf("%s: answer %q malformed", d, w)
		}
		if seen[w] {
			t.Errorf("%s: answer %q repeated within the month", d, w)
		}
		seen[w] = true
	}
	if wordleAnswer("2026-06-01") == wordleAnswer("2026-06-02") {
		t.Error("consecutive days must not share an answer")
	}
}

func TestWordleFeedback(t *testing.T) {
	// Hand-derived cases, duplicate-letter handling included: greens consume
	// their letter first, then yellows take what remains, left to right.
	cases := []struct{ answer, guess, want string }{
		// abbey vs babes: b@2 and e@3 are exact; the leading b and a are
		// present elsewhere; s is absent.
		{"abbey", "babes", "yygg-"},
		// abbey vs kebab: b@2 exact; e, a present; the final b consumes
		// abbey's one remaining b.
		{"abbey", "kebab", "-ygyy"},
		// treat vs tatty: t@0 exact; a present; the t@2 takes the one
		// remaining t, so t@3 must show absent, not yellow.
		{"treat", "tatty", "gyy--"},
		// speed vs erase: no exacts; both e's are present (speed has two),
		// the s is present, r and a absent.
		{"speed", "erase", "y--yy"},
		// speed vs eeeee: e@2 and e@3 exact; no e's remain, so every other
		// e is absent.
		{"speed", "eeeee", "--gg-"},
		{"crane", "crane", "ggggg"},
		{"crane", "xyzzz", "-----"},
	}
	for _, c := range cases {
		if got := wordleFeedback(c.answer, c.guess); got != c.want {
			t.Errorf("feedback(%q, %q) = %q, want %q", c.answer, c.guess, got, c.want)
		}
	}
}

func TestWordleHardViolation(t *testing.T) {
	// Prior "kebab" against "abbey" reveals: green b@2, yellows e, a, and a
	// second b. Hard mode then demands b in position 3 plus letters a, e,
	// and two b's somewhere.
	answer, prior := "abbey", []string{"kebab"}
	if v := wordleHardViolation(answer, prior, "bonus"); !strings.Contains(v, "must be B") {
		t.Errorf("green not reused in position should violate, got %q", v)
	}
	// "habit" keeps the green b in place but lacks the second b and the e.
	if v := wordleHardViolation(answer, prior, "habit"); !strings.Contains(v, "must contain") {
		t.Errorf("missing revealed yellow should violate, got %q", v)
	}
	if v := wordleHardViolation(answer, prior, "abbey"); v != "" {
		t.Errorf("the answer itself must satisfy hard mode, got %q", v)
	}
	if v := wordleHardViolation(answer, nil, "zonal"); v != "" {
		t.Errorf("no priors means no constraints, got %q", v)
	}
}

func TestWordleCIDStableAndOpaque(t *testing.T) {
	a, b := wordleCID("2026-06-03"), wordleCID("2026-06-03")
	if a != b || !cid.Valid(a) {
		t.Errorf("CID must be stable and valid, got %q / %q", a, b)
	}
	if wordleCID("2026-06-03") == wordleCID("2026-06-04") {
		t.Error("different days must have different CIDs")
	}
	if strings.Contains(a, wordleAnswer("2026-06-03")) {
		t.Error("the CID must not contain the answer")
	}
}

// postJSON posts a JSON body through the full route stack.
func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWordleGuessEndpoint(t *testing.T) {
	s := newSudokuTestServer(t)
	h := s.routes()
	date := "2026-06-03"
	answer := wordleAnswer(date)

	// A dictionary word that is not the answer gets a five-part feedback
	// array and no answer (the game is not over).
	probe := "crane"
	if probe == answer {
		probe = "slate"
	}
	rec := postJSON(t, h, "/wordle/"+date+"/guess", wordleGuessBody{Guess: probe})
	if rec.Code != 200 {
		t.Fatalf("valid guess = %d body %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if inv, _ := resp["invalid"].(bool); inv {
		t.Fatalf("dictionary word rejected: %s", rec.Body.String())
	}
	fb, ok := resp["feedback"].([]any)
	if !ok || len(fb) != 5 {
		t.Fatalf("feedback = %v, want a 5-element array", resp["feedback"])
	}
	if _, present := resp["answer"]; present {
		t.Error("answer must be absent while the game is live")
	}

	// Gibberish is invalid: no feedback, no guess consumed.
	rec = postJSON(t, h, "/wordle/"+date+"/guess", wordleGuessBody{Guess: "zzzzz"})
	if rec.Code != 200 {
		t.Fatalf("gibberish = %d", rec.Code)
	}
	resp = map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if inv, _ := resp["invalid"].(bool); !inv {
		t.Errorf("gibberish should be invalid: %s", rec.Body.String())
	}
	if _, has := resp["feedback"]; has {
		t.Error("an invalid guess must carry no feedback")
	}

	// Priors are validated too — a non-word prior is a malformed request.
	rec = postJSON(t, h, "/wordle/"+date+"/guess",
		wordleGuessBody{Guess: probe, Prior: []string{"zzzzz"}})
	if rec.Code != 400 {
		t.Errorf("non-word prior = %d, want 400", rec.Code)
	}

	// Solving reveals the answer.
	rec = postJSON(t, h, "/wordle/"+date+"/guess", wordleGuessBody{Guess: answer})
	resp = map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if solved, _ := resp["solved"].(bool); !solved {
		t.Fatalf("guessing the answer must solve: %s", rec.Body.String())
	}
	if got, _ := resp["answer"].(string); got != answer {
		t.Errorf("solved response answer = %q, want %q", got, answer)
	}

	// The sixth miss ends the game and reveals the answer too.
	miss := "crane"
	if miss == answer {
		miss = "slate"
	}
	prior := []string{miss, miss, miss, miss, miss}
	rec = postJSON(t, h, "/wordle/"+date+"/guess", wordleGuessBody{Guess: miss, Prior: prior})
	resp = map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if overV, _ := resp["over"].(bool); !overV {
		t.Fatalf("sixth miss must end the game: %s", rec.Body.String())
	}
	if got, _ := resp["answer"].(string); got != answer {
		t.Errorf("finished response answer = %q, want %q", got, answer)
	}

	// A seventh guess is rejected outright.
	rec = postJSON(t, h, "/wordle/"+date+"/guess",
		wordleGuessBody{Guess: miss, Prior: append(prior, miss)})
	if rec.Code != 400 {
		t.Errorf("guess after game over = %d, want 400", rec.Code)
	}
}

func TestWordleHardModeGuessRejected(t *testing.T) {
	s := newSudokuTestServer(t)
	h := s.routes()
	date := "2026-06-03"
	answer := wordleAnswer(date)

	// Find a dictionary word sharing the answer's first letter in position 1
	// — its feedback has a green there — then a word that abandons it.
	var revealing string
	for _, w := range wordleAnswers {
		if w != answer && w[0] == answer[0] {
			revealing = w
			break
		}
	}
	var contradicting string
	for _, w := range wordleAnswers {
		if w[0] != answer[0] && !strings.ContainsRune(w, rune(answer[0])) &&
			wordleHardViolation(answer, []string{revealing}, w) != "" {
			contradicting = w
			break
		}
	}
	if revealing == "" || contradicting == "" {
		t.Fatal("could not construct hard-mode fixture words")
	}

	rec := postJSON(t, h, "/wordle/"+date+"/guess",
		wordleGuessBody{Guess: contradicting, Prior: []string{revealing}, Hard: true})
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if inv, _ := resp["invalid"].(bool); !inv {
		t.Fatalf("hard-mode violation must be invalid: %s", rec.Body.String())
	}
	if _, has := resp["feedback"]; has {
		t.Error("a hard-mode rejection must carry no feedback")
	}

	// The same guess without the hard flag plays fine.
	rec = postJSON(t, h, "/wordle/"+date+"/guess",
		wordleGuessBody{Guess: contradicting, Prior: []string{revealing}})
	resp = map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if inv, _ := resp["invalid"].(bool); inv {
		t.Errorf("normal mode must accept it: %s", rec.Body.String())
	}
}

func TestWordleAPIHasNoAnswer(t *testing.T) {
	s := newSudokuTestServer(t)
	h := s.routes()
	date := "2026-06-03"
	answer := wordleAnswer(date)

	req := httptest.NewRequest("GET", "/api/wordle/"+date, nil)
	req.Header.Set("Accept-Encoding", "identity")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/api/wordle/%s = %d", date, rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp) != 2 || resp["date"] != date || resp["cid"] == "" {
		t.Errorf("public JSON = %v, want exactly {date, cid}", resp)
	}
	if strings.Contains(rec.Body.String(), answer) {
		t.Error("the answer leaked into public JSON")
	}

	// Undated form serves today.
	req = httptest.NewRequest("GET", "/api/wordle", nil)
	req.Header.Set("Accept-Encoding", "identity")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), todayUTC()) {
		t.Errorf("/api/wordle = %d body %s", rec.Code, rec.Body.String())
	}
}

func TestWordlePageRenders(t *testing.T) {
	s := newSudokuTestServer(t)
	h := s.routes()
	answer := wordleAnswer(todayUTC())

	req := httptest.NewRequest("GET", "/wordle", nil)
	req.Header.Set("Accept-Encoding", "identity")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "wordle-grid") {
		t.Errorf("/wordle = %d, grid present: %v", rec.Code,
			strings.Contains(rec.Body.String(), "wordle-grid"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/static/wordle.js?v="+wordleJSVer) {
		t.Error("page must link the fingerprinted wordle.js")
	}
	if strings.Contains(body, "Log in") || strings.Contains(body, "/login") {
		t.Error("the page must carry no login affordance")
	}
	if strings.Contains(body, "treak") { // Streak/streak
		t.Error("the page must not show a streak")
	}
	if strings.Contains(body, strings.ToUpper(answer)) {
		t.Error("the page must not contain the answer")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=600" {
		t.Errorf("wordle page cache-control = %q, want public, max-age=600", cc)
	}

	// Out-of-range dates do not exist.
	for _, path := range []string{"/wordle/2019-12-31", "/wordle/2199-01-01", "/wordle/garbage"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}
