package main

import (
	"math/bits"
	"math/rand/v2"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
)

// The sudoku artifact — one puzzle a day, derived purely from the date. A
// ChaCha8 stream seeded from sha256("sudoku:"+date) drives a randomized
// backtracking fill to a full valid grid, then punches holes in 180°
// rotationally symmetric pairs while a counting solver guarantees the puzzle
// keeps exactly one solution. The full solution is re-derivable from the same
// seed at any time, so it is never stored and never sent to a client —
// solve-state validation re-derives it server-side.

// sudokuClueTargets is the difficulty ramp, indexed by time.Weekday
// (Sunday=0): Monday is easiest at 40 clues and each day sheds a few down to
// Sunday's 26. The hole punch stops at the target, or earlier when no further
// symmetric removal can keep the solution unique — deterministic either way.
var sudokuClueTargets = [7]int{26, 40, 38, 35, 32, 30, 28}

// sudokuPuzzle is the public face of one day's puzzle — everything here may
// appear in JSON. The solution deliberately is not part of it.
type sudokuPuzzle struct {
	Date       string
	Clues      string // 81 chars, '1'-'9' for givens, '0' for empties
	ClueCount  int
	Difficulty string // "1/7" (Monday, easiest) … "7/7" (Sunday, hardest)
	Weekday    string
	CID        string // CID of the clues string
}

// sudokuDayValid reports whether a date has a puzzle: well-formed, on or
// after the artifact epoch, not in the future.
func sudokuDayValid(date string) bool {
	return validDate(date) && date >= artifactEpoch && date <= todayUTC()
}

// sudokuFor derives one day's public puzzle.
func sudokuFor(date string) sudokuPuzzle {
	p, _ := sudokuDerive(date)
	return p
}

// sudokuDerive derives one day's puzzle and its solution — pure, no storage.
// The solution stays inside the server: handlers use it to validate posted
// entries and must never serialize it.
func sudokuDerive(date string) (sudokuPuzzle, [81]byte) {
	rng := newRNG(domainSudoku, date)
	sol := sudokuSolution(rng)

	// Hole sites under 180° rotational symmetry: 40 cell pairs (i, 80-i)
	// plus the lone center cell, visited in seed-shuffled order.
	order := make([]int, 41)
	for i := range order {
		order[i] = i
	}
	shuffleInts(rng, order)

	wd := sudokuWeekday(date)
	target := sudokuClueTargets[wd]
	grid := sol
	clues := 81
	for _, site := range order {
		if clues <= target {
			break
		}
		a, b := site, 80-site
		sa, sb := grid[a], grid[b]
		removed := 1
		grid[a] = 0
		if b != a {
			grid[b] = 0
			removed = 2
		}
		// Keep unique solvability: revert any removal that opens a second
		// solution (the counting solver stops as soon as it finds two).
		if countSolutions(&grid, 2) != 1 {
			grid[a], grid[b] = sa, sb
			continue
		}
		clues -= removed
	}

	cluesStr := gridString(grid)
	return sudokuPuzzle{
		Date:       date,
		Clues:      cluesStr,
		ClueCount:  clues,
		Difficulty: sudokuDifficulty(wd),
		Weekday:    wd.String(),
		CID:        cid.Of([]byte(cluesStr)),
	}, sol
}

// sudokuWeekday returns the weekday of a date; malformed dates were filtered
// upstream, so the zero time's weekday is an acceptable degenerate answer.
func sudokuWeekday(date string) time.Weekday {
	d, _ := time.Parse(dateLayout, date)
	return d.Weekday()
}

// sudokuDifficulty maps a weekday to its "rank/7" label — Monday 1 (easiest)
// through Sunday 7 (hardest).
func sudokuDifficulty(wd time.Weekday) string {
	rank := (int(wd)+6)%7 + 1
	return string('0'+byte(rank)) + "/7"
}

// shuffleInts is a Fisher–Yates shuffle drawing from the artifact stream.
func shuffleInts(rng *rand.ChaCha8, s []int) {
	for i := len(s) - 1; i > 0; i-- {
		j := int(rng.Uint64() % uint64(i+1))
		s[i], s[j] = s[j], s[i]
	}
}

// gridString renders a grid as the 81-char wire form: '0' for empty.
func gridString(g [81]byte) string {
	b := make([]byte, 81)
	for i, d := range g {
		b[i] = '0' + d
	}
	return string(b)
}

// ── full-solution generation ───────────────────────────────────────────────

// sudokuSolution fills a complete valid grid by backtracking, trying digits
// at each cell in a per-cell order shuffled from the seed stream. The orders
// are drawn up front, so the backtracking path — and the solution — is a
// fixed function of the stream.
func sudokuSolution(rng *rand.ChaCha8) [81]byte {
	var orders [81][9]byte
	for i := range orders {
		for d := byte(0); d < 9; d++ {
			orders[i][d] = d + 1
		}
		for j := 8; j > 0; j-- {
			k := int(rng.Uint64() % uint64(j+1))
			orders[i][j], orders[i][k] = orders[i][k], orders[i][j]
		}
	}
	var g [81]byte
	var fill func(i int) bool
	fill = func(i int) bool {
		if i == 81 {
			return true
		}
		for _, d := range orders[i] {
			if canPlace(&g, i, d) {
				g[i] = d
				if fill(i + 1) {
					return true
				}
				g[i] = 0
			}
		}
		return false
	}
	fill(0) // an empty grid always fills, whatever the digit orders
	return g
}

// canPlace reports whether digit d may go in cell i without clashing with
// its row, column, or 3×3 box.
func canPlace(g *[81]byte, i int, d byte) bool {
	r, c := i/9, i%9
	br, bc := r/3*3, c/3*3
	for k := 0; k < 9; k++ {
		if g[r*9+k] == d || g[k*9+c] == d || g[(br+k/3)*9+bc+k%3] == d {
			return false
		}
	}
	return true
}

// ── counting solver ────────────────────────────────────────────────────────

// countSolutions counts the solutions of a partial grid, stopping at limit —
// the hole punch only ever needs to know "exactly one" versus "two or more".
// Bitmask constraint tracking plus most-constrained-cell selection keeps it
// fast enough to run after every tentative removal. g is restored before
// returning.
func countSolutions(g *[81]byte, limit int) int {
	var rows, cols, boxes [9]uint16
	for i, d := range g {
		if d == 0 {
			continue
		}
		bit := uint16(1) << (d - 1)
		r, c := i/9, i%9
		bx := r/3*3 + c/3
		if rows[r]&bit != 0 || cols[c]&bit != 0 || boxes[bx]&bit != 0 {
			return 0 // the givens already contradict
		}
		rows[r] |= bit
		cols[c] |= bit
		boxes[bx] |= bit
	}

	count := 0
	var rec func()
	rec = func() {
		// Most-constrained empty cell first; a cell with no candidates
		// prunes the branch immediately.
		best, bestMask, bestN := -1, uint16(0), 10
		for i, d := range g {
			if d != 0 {
				continue
			}
			r, c := i/9, i%9
			bx := r/3*3 + c/3
			mask := ^(rows[r] | cols[c] | boxes[bx]) & 0x1ff
			n := bits.OnesCount16(mask)
			if n == 0 {
				return // dead end
			}
			if n < bestN {
				best, bestMask, bestN = i, mask, n
				if n == 1 {
					break
				}
			}
		}
		if best == -1 {
			count++ // no empty cells — a full solution
			return
		}
		r, c := best/9, best%9
		bx := r/3*3 + c/3
		for mask := bestMask; mask != 0; mask &= mask - 1 {
			bit := mask & -mask
			g[best] = byte(bits.TrailingZeros16(bit)) + 1
			rows[r] |= bit
			cols[c] |= bit
			boxes[bx] |= bit
			rec()
			rows[r] &^= bit
			cols[c] &^= bit
			boxes[bx] &^= bit
			g[best] = 0
			if count >= limit {
				return
			}
		}
	}
	rec()
	return count
}
