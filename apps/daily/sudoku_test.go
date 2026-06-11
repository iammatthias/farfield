package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

// sudokuTestWeek is one full past week, Monday through Sunday.
var sudokuTestWeek = []string{
	"2026-06-01", "2026-06-02", "2026-06-03", "2026-06-04",
	"2026-06-05", "2026-06-06", "2026-06-07",
}

func TestSudokuDeterminism(t *testing.T) {
	a := sudokuFor("2026-06-03")
	b := sudokuFor("2026-06-03")
	if a.Clues != b.Clues {
		t.Error("same date must derive identical clues")
	}
	if a.CID != b.CID || !cid.Valid(a.CID) {
		t.Errorf("same date must derive an identical, valid CID; got %q and %q", a.CID, b.CID)
	}
	if c := sudokuFor("2026-06-04"); c.Clues == a.Clues {
		t.Error("different dates should not share a puzzle")
	}
}

func TestSudokuUniqueSolvability(t *testing.T) {
	for _, date := range sudokuTestWeek {
		p, sol := sudokuDerive(date)
		if len(p.Clues) != 81 {
			t.Fatalf("%s: clues length = %d, want 81", date, len(p.Clues))
		}
		var grid [81]byte
		givens := 0
		for i := range 81 {
			d := p.Clues[i]
			if d < '0' || d > '9' {
				t.Fatalf("%s: clue %d is %q, want a digit", date, i, d)
			}
			grid[i] = d - '0'
			if d != '0' {
				givens++
				if sol[i] != d-'0' {
					t.Fatalf("%s: given %d disagrees with the solution", date, i)
				}
				// 180° rotational symmetry: a given's mirror is a given.
				if p.Clues[80-i] == '0' {
					t.Errorf("%s: given %d has no rotational partner", date, i)
				}
			}
		}
		if givens != p.ClueCount {
			t.Errorf("%s: ClueCount %d, counted %d", date, p.ClueCount, givens)
		}
		if n := countSolutions(&grid, 2); n != 1 {
			t.Errorf("%s: %d solutions, want exactly 1", date, n)
		}
		if n, ok := validSolution(sol); !ok {
			t.Errorf("%s: derived solution invalid at cell %d", date, n)
		}
	}
}

// validSolution checks a full grid satisfies all sudoku constraints.
func validSolution(g [81]byte) (int, bool) {
	for i := range 81 {
		d := g[i]
		if d < 1 || d > 9 {
			return i, false
		}
		g[i] = 0
		ok := canPlace(&g, i, d)
		g[i] = d
		if !ok {
			return i, false
		}
	}
	return 0, true
}

func TestSudokuDifficultyRamp(t *testing.T) {
	mon := sudokuFor(sudokuTestWeek[0])
	sun := sudokuFor(sudokuTestWeek[6])
	if mon.ClueCount <= sun.ClueCount {
		t.Errorf("Monday (%d clues) should be easier than Sunday (%d clues)",
			mon.ClueCount, sun.ClueCount)
	}
	if mon.Difficulty != "1/7" || sun.Difficulty != "7/7" {
		t.Errorf("difficulty labels = %q and %q, want 1/7 and 7/7",
			mon.Difficulty, sun.Difficulty)
	}
}

// newSudokuTestServer builds a full server on a temp database, offline.
func newSudokuTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "daily.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	tmpl, err := web.ParseTemplates(assets, templateFuncs)
	if err != nil {
		t.Fatalf("templates: %v", err)
	}
	s := &Server{
		db:      db,
		fetcher: newFetcher("DEMO_KEY"),
		rd:      &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
		auth:    &web.Auth{DB: db, Password: "t"},
	}
	s.fetcher.noteNASAError() // keep the test offline
	return s
}

// loginCookie opens a session directly in the store and returns its cookie.
func loginCookie(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	token := auth.NewSessionToken()
	if err := store.InsertSession(s.db, token, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return auth.SessionCookie(token, false)
}

func TestSudokuAPIJSONShape(t *testing.T) {
	s := newSudokuTestServer(t)
	h := s.routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/sudoku/2026-06-03", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, leaked := body["solution"]; leaked {
		t.Fatal("the solution must never appear in API JSON")
	}
	clues, _ := body["clues"].(string)
	if len(clues) != 81 || strings.Trim(clues, "0123456789") != "" {
		t.Errorf("clues = %q, want 81 digit chars", clues)
	}
	if got, _ := body["cid"].(string); !cid.Valid(got) {
		t.Errorf("cid = %q, want a valid CID", got)
	}
	if got, _ := body["date"].(string); got != "2026-06-03" {
		t.Errorf("date = %q", got)
	}
	if got, _ := body["difficulty"].(string); got != "3/7" {
		t.Errorf("difficulty = %q, want 3/7 for a Wednesday", got)
	}

	// the undated endpoint serves today's puzzle
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/sudoku", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("undated status = %d, want 200", rec.Code)
	}
}

func TestSudokuStatePost(t *testing.T) {
	s := newSudokuTestServer(t)
	h := s.routes()
	date := "2026-06-03"
	p, sol := sudokuDerive(date)
	solution := gridString(sol)

	post := func(entries string, ck *http.Cookie) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"entries": entries, "solveMs": 90000})
		req := httptest.NewRequest("POST", "/sudoku/"+date+"/state", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if ck != nil {
			req.AddCookie(ck)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Unauthed → 401 JSON.
	if rec := post(p.Clues, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthed status = %d, want 401", rec.Code)
	}

	ck := loginCookie(t, s)

	// Partial save persists, unsolved, no conflicts.
	partial := []byte(p.Clues)
	var open []int // cells the player fills
	for i := range 81 {
		if p.Clues[i] == '0' {
			open = append(open, i)
		}
	}
	partial[open[0]] = solution[open[0]]
	rec := post(string(partial), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial save status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var res struct {
		Solved    bool  `json:"solved"`
		Conflicts []int `json:"conflicts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Solved || len(res.Conflicts) != 0 {
		t.Errorf("partial save = %+v, want unsolved with no conflicts", res)
	}
	st, err := getSolveState(s.db, domainSudoku, date)
	if err != nil || st == nil || st.Payload != string(partial) || st.Solved || st.SolveMs != 90000 {
		t.Fatalf("persisted state = %+v, err %v", st, err)
	}

	// Complete and correct → solved.
	rec = post(solution, ck)
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || !res.Solved || len(res.Conflicts) != 0 {
		t.Fatalf("correct grid: status %d, result %+v", rec.Code, res)
	}
	if st, _ := getSolveState(s.db, domainSudoku, date); st == nil || !st.Solved {
		t.Fatal("solved state should persist")
	}

	// Complete but wrong → unsolved, with the wrong cell in conflicts.
	wrong := []byte(solution)
	i := open[0]
	wrong[i] = '1' + (wrong[i]-'1'+1)%9 // a different digit, still 1..9
	rec = post(string(wrong), ck)
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || res.Solved {
		t.Fatalf("wrong grid: status %d, result %+v", rec.Code, res)
	}
	found := false
	for _, c := range res.Conflicts {
		if c == i {
			found = true
		}
	}
	if !found {
		t.Errorf("conflicts %v should include the wrong cell %d", res.Conflicts, i)
	}

	// Entries inconsistent with the givens → 400.
	bad := []byte(solution)
	for j := range 81 {
		if p.Clues[j] != '0' {
			bad[j] = '1' + (bad[j]-'1'+1)%9
			break
		}
	}
	if rec := post(string(bad), ck); rec.Code != http.StatusBadRequest {
		t.Errorf("givens-violating entries: status %d, want 400", rec.Code)
	}
}

func TestSudokuPageRendersAndRestoresState(t *testing.T) {
	s := newSudokuTestServer(t)
	h := s.routes()
	date := "2026-06-03"
	p, sol := sudokuDerive(date)

	// Unauthed page renders the grid and the login hint.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/sudoku/"+date, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("page status = %d, want 200", rec.Code)
	}
	html := rec.Body.String()
	if !strings.Contains(html, `id="sudoku-grid"`) {
		t.Error("page should contain the grid")
	}
	if !strings.Contains(html, "save progress") {
		t.Error("unauthed page should hint at logging in")
	}
	if strings.Contains(html, gridString(sol)) {
		t.Error("the solution string must never reach the page")
	}

	// A saved entry is restored into the grid when authed.
	var i int
	for i = range 81 {
		if p.Clues[i] == '0' {
			break
		}
	}
	entries := []byte(p.Clues)
	entries[i] = sol[i] + '0'
	if err := upsertSolveState(s.db, &solveState{
		Domain: domainSudoku, Date: date, Payload: string(entries),
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	req := httptest.NewRequest("GET", "/sudoku/"+date, nil)
	req.AddCookie(loginCookie(t, s))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	want := fmt.Sprintf(`value="%c"`, entries[i])
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("authed page should restore the saved entry (%s)", want)
	}
}

func TestSolveStreak(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "daily.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	today := todayUTC()
	seed := func(date string, solved bool) {
		t.Helper()
		if err := upsertSolveState(db, &solveState{
			Domain: domainSudoku, Date: date, Payload: "", Solved: solved,
		}); err != nil {
			t.Fatalf("seed %s: %v", date, err)
		}
	}

	if n, _ := solveStreak(db, domainSudoku, today); n != 0 {
		t.Errorf("empty table streak = %d, want 0", n)
	}

	// Three consecutive solved days ending today.
	seed(today, true)
	seed(addDays(today, -1), true)
	seed(addDays(today, -2), true)
	if n, _ := solveStreak(db, domainSudoku, today); n != 3 {
		t.Errorf("streak = %d, want 3", n)
	}

	// A gap stops the count.
	seed(addDays(today, -4), true)
	if n, _ := solveStreak(db, domainSudoku, today); n != 3 {
		t.Errorf("streak across a gap = %d, want 3", n)
	}

	// An unsolved day breaks the run at the break point.
	seed(addDays(today, -1), false)
	if n, _ := solveStreak(db, domainSudoku, today); n != 1 {
		t.Errorf("streak with unsolved yesterday = %d, want 1", n)
	}

	// Today still pending: the run ending yesterday counts.
	seed(addDays(today, -1), true)
	seed(today, false)
	if n, _ := solveStreak(db, domainSudoku, today); n != 2 {
		t.Errorf("pending-today streak = %d, want 2 (yesterday + day before)", n)
	}

	// Other domains do not contribute.
	seed2 := &solveState{Domain: "wordle", Date: today, Solved: true}
	_ = upsertSolveState(db, seed2)
	if n, _ := solveStreak(db, "wordle", today); n != 1 {
		t.Errorf("wordle streak = %d, want 1", n)
	}
}
