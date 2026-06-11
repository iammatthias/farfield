package main

import (
	"database/sql"
	"errors"

	"github.com/iammatthias/farfield/lib/store"
)

// Solve state — the first stateful piece of daily. The instance is
// single-user: once a visitor authenticates, their progress is the instance's
// progress, so rows key on (domain, date) with no user column. The domain
// column is shared by sudoku and (soon) wordle.

// solveStateSchema holds per-day play progress for the interactive artifacts.
const solveStateSchema = `
CREATE TABLE IF NOT EXISTS solve_state (
	domain     TEXT NOT NULL,
	date       TEXT NOT NULL,
	payload    TEXT NOT NULL DEFAULT '',
	solved     INTEGER NOT NULL DEFAULT 0,
	solve_ms   INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (domain, date)
);`

// solveState is one day's saved progress for one artifact domain. Payload is
// artifact-defined — for sudoku, the 81-char entries string.
type solveState struct {
	Domain    string
	Date      string
	Payload   string
	Solved    bool
	SolveMs   int64
	UpdatedAt string
}

// getSolveState returns the saved state for (domain, date), or (nil, nil).
func getSolveState(db *sql.DB, domain, date string) (*solveState, error) {
	var st solveState
	var solved int
	err := db.QueryRow(
		`SELECT domain, date, payload, solved, solve_ms, updated_at
		 FROM solve_state WHERE domain = ? AND date = ?`, domain, date).
		Scan(&st.Domain, &st.Date, &st.Payload, &solved, &st.SolveMs, &st.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st.Solved = solved != 0
	return &st, nil
}

// upsertSolveState inserts or replaces the state for (domain, date), stamping
// the update time.
func upsertSolveState(db *sql.DB, st *solveState) error {
	st.UpdatedAt = store.NowRFC3339()
	solved := 0
	if st.Solved {
		solved = 1
	}
	_, err := db.Exec(
		`INSERT INTO solve_state (domain, date, payload, solved, solve_ms, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(domain, date) DO UPDATE SET
		   payload=excluded.payload, solved=excluded.solved,
		   solve_ms=excluded.solve_ms, updated_at=excluded.updated_at`,
		st.Domain, st.Date, st.Payload, solved, st.SolveMs, st.UpdatedAt)
	return err
}

// solveStreak returns the run of consecutive solved days for a domain ending
// at today. Today itself may still be pending: when today is unsolved but
// yesterday is solved, the run ending yesterday counts — an in-progress day
// does not zero the streak.
func solveStreak(db *sql.DB, domain, today string) (int, error) {
	rows, err := db.Query(
		`SELECT date FROM solve_state
		 WHERE domain = ? AND solved = 1 AND date <= ?
		 ORDER BY date DESC`, domain, today)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	streak := 0
	expect := today
	first := true
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return 0, err
		}
		if first && d == addDays(today, -1) {
			expect = d // today unsolved so far — count the run through yesterday
		}
		first = false
		if d != expect {
			break
		}
		streak++
		expect = addDays(expect, -1)
	}
	return streak, rows.Err()
}
