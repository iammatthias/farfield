package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// hubGet runs one GET through the full route stack.
func hubGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Accept-Encoding", "identity")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func TestHubToday(t *testing.T) {
	s := newSudokuTestServer(t)
	rec := hubGet(t, s, "/")
	if rec.Code != 200 {
		t.Fatalf("/ = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, card := range []string{"Photo", "Art", "Sudoku", "Wordle"} {
		if !strings.Contains(body, ">"+card+"</span>") {
			t.Errorf("hub is missing the %s card", card)
		}
	}
	if !strings.Contains(body, todayUTC()) {
		t.Error("hub should carry today's date")
	}
	if !strings.Contains(body, "/art/"+todayUTC()+".svg") {
		t.Error("hub art card should embed the day's SVG plate")
	}
	// Every HTML page embeds session state (nav login/logout, solve status),
	// so even the anonymous hub must never be shared-cacheable.
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-cache" {
		t.Errorf("hub cache-control = %q, want private, no-cache", cc)
	}
	if rec.Header().Get("ETag") != "" {
		t.Error("HTML pages must not carry an ETag")
	}
}

func TestHubPastAndOutOfRange(t *testing.T) {
	s := newSudokuTestServer(t)

	// A past date in range renders, linked back to its artifacts.
	rec := hubGet(t, s, "/2026-06-01")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/sudoku/2026-06-01") {
		t.Errorf("/2026-06-01 = %d, sudoku link present: %v",
			rec.Code, strings.Contains(rec.Body.String(), "/sudoku/2026-06-01"))
	}

	// Pre-epoch, future, and non-date strays all 404.
	for _, path := range []string{"/2019-01-01", "/2999-01-01", "/favicon.ico", "/garbage"} {
		if rec := hubGet(t, s, path); rec.Code != 404 {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}

// TestHubRouting pins down the ServeMux precedence the hub relies on: the
// GET /{date} wildcard must not shadow any literal sibling route.
func TestHubRouting(t *testing.T) {
	s := newSudokuTestServer(t)
	cases := map[string]string{
		"/photo":             "Astronomy Picture", // the photo page eyebrow
		"/art":               "HYPERSTRUCTURE",    // the art page eyebrow
		"/sudoku":            "sudoku-grid",       // the sudoku grid
		"/wordle":            "wordle-grid",       // the wordle grid
		"/login":             "password",          // the login form
		"/status":            `"service"`,         // JSON status
		"/api/wordle":        `"cid"`,             // public wordle JSON
		"/static/styles.css": "--ink",             // the shared theme
		"/static/wordle.js":  "wordle grid behavior",
	}
	for path, marker := range cases {
		rec := hubGet(t, s, path)
		if rec.Code != 200 {
			t.Errorf("%s = %d, want 200 (shadowed by /{date}?)", path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), marker) {
			t.Errorf("%s does not look like its own page (marker %q missing)", path, marker)
		}
	}

	// The old / → /photo redirect is gone; / is the hub itself.
	if rec := hubGet(t, s, "/"); rec.Code != 200 {
		t.Errorf("/ = %d, want the hub, not a redirect", rec.Code)
	}
	// Deeper legacy redirects still work.
	if rec := hubGet(t, s, "/archive"); rec.Code != 301 {
		t.Errorf("/archive = %d, want 301", rec.Code)
	}
}

func TestHubShowsSolveStateWhenAuthed(t *testing.T) {
	s := newSudokuTestServer(t)
	cookie := loginCookie(t, s)
	date := "2026-06-01"
	if err := upsertSolveState(s.db, &solveState{
		Domain: domainWordle, Date: date, Payload: `{"guesses":[],"hard":false}`,
		Solved: true, SolveMs: 61000,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	req := httptest.NewRequest("GET", "/"+date, nil)
	req.Header.Set("Accept-Encoding", "identity")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "solved · 1:01") {
		t.Errorf("authed hub = %d, solved line present: %v", rec.Code,
			strings.Contains(body, "solved · 1:01"))
	}
	if !strings.Contains(body, "unplayed") {
		t.Error("authed hub should mark the unplayed sudoku")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("authed hub cache-control = %q, want private", cc)
	}

	// The anonymous hub shows no play state.
	if rec := hubGet(t, s, "/"+date); strings.Contains(rec.Body.String(), "unplayed") {
		t.Error("anonymous hub must not show play state")
	}
}
