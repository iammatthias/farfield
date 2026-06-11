package main

// 4-D Hilbert curve, both directions, after John Skilling, "Programming the
// Hilbert curve", AIP Conf. Proc. 707, 381 (2004) — the compact
// transposed-axes formulation of the Butz algorithm. The Hilbert index is
// carried "transposed": its bits interleaved across the four axis words, most
// significant bit in axis 0. Decoding Gray-decodes that vector and unrolls the
// per-level rotations/reflections in place; encoding runs the same loops
// backwards. No tables, no recursion, O(order) per conversion.

// hilbert4d maps a Hilbert-curve index to its 4-D coordinate for a curve of
// the given order (side 2^order per axis, 2^(4·order) cells total).
// Consecutive indices always map to lattice neighbors — coordinates that
// differ by exactly 1 on exactly 1 axis.
func hilbert4d(n uint64, order int) (x, y, z, w int) {
	X := indexToTranspose(n, order)
	transposeToAxes(X[:], order)
	return int(X[0]), int(X[1]), int(X[2]), int(X[3])
}

// hilbert4dInverse maps a 4-D coordinate back to its Hilbert index — the
// exact inverse of hilbert4d: hilbert4dInverse(hilbert4d(n)) == n.
func hilbert4dInverse(x, y, z, w, order int) uint64 {
	X := [4]uint32{uint32(x), uint32(y), uint32(z), uint32(w)}
	axesToTranspose(X[:], order)
	return transposeToIndex(X, order)
}

// indexToTranspose splits the 4·b bits of n across four axis words, most
// significant index bit first, round-robin: index bit k (from the top) lands
// in axis k mod 4 — Skilling's transposed form.
func indexToTranspose(n uint64, b int) [4]uint32 {
	var X [4]uint32
	total := 4 * b
	for p := 0; p < total; p++ {
		bit := (n >> uint(total-1-p)) & 1
		X[p%4] |= uint32(bit) << uint(b-1-p/4)
	}
	return X
}

// transposeToIndex re-interleaves four axis words into one index — the exact
// inverse of indexToTranspose.
func transposeToIndex(X [4]uint32, b int) uint64 {
	var n uint64
	total := 4 * b
	for p := 0; p < total; p++ {
		bit := uint64(X[p%4]>>uint(b-1-p/4)) & 1
		n |= bit << uint(total-1-p)
	}
	return n
}

// transposeToAxes converts a transposed Hilbert index into plain coordinates
// in place (Skilling 2004, TransposeToAxes).
func transposeToAxes(X []uint32, b int) {
	n := len(X)
	// Gray decode by H ^ (H/2), the division borrow carried across words.
	t := X[n-1] >> 1
	for i := n - 1; i > 0; i-- {
		X[i] ^= X[i-1]
	}
	X[0] ^= t
	// Undo the excess rotations and reflections applied at each level.
	for q := uint32(2); q != uint32(1)<<uint(b); q <<= 1 {
		p := q - 1
		for i := n - 1; i >= 0; i-- {
			if X[i]&q != 0 {
				X[0] ^= p // invert low bits of X[0]
			} else { // exchange low bits of X[i] and X[0]
				t := (X[0] ^ X[i]) & p
				X[0] ^= t
				X[i] ^= t
			}
		}
	}
}

// axesToTranspose converts plain coordinates into a transposed Hilbert index
// in place (Skilling 2004, AxesToTranspose) — transposeToAxes run backwards.
func axesToTranspose(X []uint32, b int) {
	n := len(X)
	m := uint32(1) << uint(b-1)
	// Apply the per-level rotations and reflections, top bit down.
	for q := m; q > 1; q >>= 1 {
		p := q - 1
		for i := 0; i < n; i++ {
			if X[i]&q != 0 {
				X[0] ^= p
			} else {
				t := (X[0] ^ X[i]) & p
				X[0] ^= t
				X[i] ^= t
			}
		}
	}
	// Gray encode.
	for i := 1; i < n; i++ {
		X[i] ^= X[i-1]
	}
	var t uint32
	for q := m; q > 1; q >>= 1 {
		if X[n-1]&q != 0 {
			t ^= q - 1
		}
	}
	for i := 0; i < n; i++ {
		X[i] ^= t
	}
}
