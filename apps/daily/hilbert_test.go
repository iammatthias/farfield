package main

import "testing"

func absInt(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

// TestHilbertAdjacency: consecutive indices must map to lattice neighbors —
// coordinates differing by exactly 1 on exactly 1 axis.
func TestHilbertAdjacency(t *testing.T) {
	px, py, pz, pw := hilbert4d(0, artOrder)
	for n := uint64(1); n <= 5000; n++ {
		x, y, z, w := hilbert4d(n, artOrder)
		if d := absInt(x-px) + absInt(y-py) + absInt(z-pz) + absInt(w-pw); d != 1 {
			t.Fatalf("n=%d: [%d %d %d %d] -> [%d %d %d %d] moved %d, want 1",
				n, px, py, pz, pw, x, y, z, w, d)
		}
		px, py, pz, pw = x, y, z, w
	}
}

// TestHilbertBijectionOrder2: every index of an order-2 curve must land on a
// distinct in-range coordinate — 256 indices, 256 cells, no repeats.
func TestHilbertBijectionOrder2(t *testing.T) {
	seen := make(map[[4]int]bool, 256)
	for n := uint64(0); n < 256; n++ {
		x, y, z, w := hilbert4d(n, 2)
		for _, v := range []int{x, y, z, w} {
			if v < 0 || v >= 4 {
				t.Fatalf("n=%d: coordinate %d out of [0,4)", n, v)
			}
		}
		c := [4]int{x, y, z, w}
		if seen[c] {
			t.Fatalf("n=%d: coordinate %v repeated", n, c)
		}
		seen[c] = true
	}
	if len(seen) != 256 {
		t.Fatalf("got %d distinct cells, want 256", len(seen))
	}
}

// TestHilbertInverse: the inverse must undo the forward mapping for every
// index of the full order-4 curve.
func TestHilbertInverse(t *testing.T) {
	for n := uint64(0); n < artCells; n++ {
		x, y, z, w := hilbert4d(n, artOrder)
		if got := hilbert4dInverse(x, y, z, w, artOrder); got != n {
			t.Fatalf("inverse(forward(%d)) = %d", n, got)
		}
	}
}

// TestHilbertGolden pins the exact curve: a future "improvement" to the
// algorithm would silently re-derive every plate ever published.
func TestHilbertGolden(t *testing.T) {
	golden := []struct {
		n     uint64
		coord [4]int
	}{
		{0, [4]int{0, 0, 0, 0}},
		{1, [4]int{1, 0, 0, 0}},
		{2, [4]int{1, 0, 0, 1}},
		{15, [4]int{0, 1, 0, 0}},
		{255, [4]int{0, 0, 3, 0}},
		{2353, [4]int{4, 0, 5, 6}},
		{65535, [4]int{15, 0, 0, 0}},
	}
	for _, g := range golden {
		x, y, z, w := hilbert4d(g.n, artOrder)
		if c := [4]int{x, y, z, w}; c != g.coord {
			t.Errorf("hilbert4d(%d) = %v, want %v", g.n, c, g.coord)
		}
	}
}
