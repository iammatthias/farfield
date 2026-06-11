package main

import "math/rand/v2"

// terrainParams shape one biome's heightfield: how many octaves of value
// noise stack up, the lattice frequency the first octave samples at, and how
// fast amplitudes decay. Different profiles make regions read differently —
// broad flats, rolling dunes, broken ridges.
type terrainParams struct {
	Octaves     int     // noise layers (3–4)
	BaseFreq    int     // lattice cells across the field at octave 0
	Persistence float64 // amplitude falloff per octave
}

// heightfield builds a size×size terrain quantized into [0, levels) elevation
// steps. Octaves of value noise — random lattice values interpolated with
// smoothstep — are layered with per-octave amplitude decay, then the field is
// stretched to full range and quantized so every plate uses its whole glyph
// ramp. Every draw comes from rng in a fixed order, so the result is a pure
// function of the seed.
func heightfield(rng *rand.ChaCha8, p terrainParams, size, levels int) [][]int {
	sum := make([][]float64, size)
	for i := range sum {
		sum[i] = make([]float64, size)
	}
	amp := 1.0
	for o := 0; o < p.Octaves; o++ {
		cells := p.BaseFreq << o
		lat := lattice(rng, cells+1)
		for r := 0; r < size; r++ {
			fy := float64(r) / float64(size) * float64(cells)
			for c := 0; c < size; c++ {
				fx := float64(c) / float64(size) * float64(cells)
				// The explicit conversion pins evaluation so the compiler
				// cannot fuse this multiply into the add (FMA) — heights must
				// quantize identically on every architecture.
				sum[r][c] += float64(amp * noiseAt(lat, fx, fy))
			}
		}
		amp *= p.Persistence
	}
	lo, hi := sum[0][0], sum[0][0]
	for r := 0; r < size; r++ {
		for c := 0; c < size; c++ {
			if sum[r][c] < lo {
				lo = sum[r][c]
			}
			if sum[r][c] > hi {
				hi = sum[r][c]
			}
		}
	}
	out := make([][]int, size)
	for r := range out {
		out[r] = make([]int, size)
		for c := range out[r] {
			h := 0.0
			if hi > lo {
				h = (sum[r][c] - lo) / (hi - lo)
			}
			lv := int(h * float64(levels))
			if lv >= levels {
				lv = levels - 1
			}
			out[r][c] = lv
		}
	}
	return out
}

// lattice draws an n×n grid of uniform values from the stream, row-major.
func lattice(rng *rand.ChaCha8, n int) [][]float64 {
	g := make([][]float64, n)
	for i := range g {
		g[i] = make([]float64, n)
		for j := range g[i] {
			g[i][j] = randFloat(rng)
		}
	}
	return g
}

// noiseAt samples the lattice at a fractional position — bilinear
// interpolation with smoothstep easing, the classic value-noise kernel.
func noiseAt(g [][]float64, fx, fy float64) float64 {
	x0, y0 := int(fx), int(fy)
	tx, ty := smoothstep(fx-float64(x0)), smoothstep(fy-float64(y0))
	a := lerp(g[y0][x0], g[y0][x0+1], tx)
	b := lerp(g[y0+1][x0], g[y0+1][x0+1], tx)
	return lerp(a, b, ty)
}

// smoothstep is the cubic 3t²−2t³ ease. The explicit conversion blocks FMA
// fusion (see heightfield).
func smoothstep(t float64) float64 { return float64(t*t) * (3 - 2*t) }

// lerp interpolates a→b by t, conversion-pinned against FMA fusion.
func lerp(a, b, t float64) float64 { return a + float64(t*(b-a)) }
