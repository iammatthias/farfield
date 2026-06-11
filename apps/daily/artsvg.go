package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// SVG rendering for the art artifact. Both renderers emit byte-deterministic
// output: iteration orders are fixed (loops and an explicit sort, never bare
// map ranges) and every coordinate is integer math, so no float formatting
// instability can leak into the bytes — the CID of a date's SVG is stable
// forever.

// svgFont is the monospace stack — the SVG equivalent of the theme's
// --font-mono.
const svgFont = "ui-monospace, 'SF Mono', Menlo, Consolas, monospace"

// Plot projection — the 24×24 heightfield drawn isometrically:
// x' = (col−row)·dx, y' = (col+row)·dy − height·dz, all integers.
const (
	plotSize = 24  // heightfield side
	plotDX   = 10  // half-width of one cell step
	plotDY   = 5   // half-height of one cell step
	plotDZ   = 4   // vertical lift per elevation level
	plotOX   = 250 // isometric origin
	plotOY   = 80
)

// renderPlotSVG draws one day's plate: the heightfield as an isometric
// character-grid terrain, glyph and ink chosen by elevation band, with a
// survey-plate title block in the corner.
func renderPlotSVG(date string, n uint64, c [4]int, biomeIdx int, b Biome, hf [][]int) []byte {
	var buf bytes.Buffer
	buf.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 500 340" role="img" aria-label="Generative terrain plate for ` + date + `">` + "\n")

	// Title block — small mono uppercase, like a survey plate annotation.
	fmt.Fprintf(&buf, `<g font-family="%s" font-size="9" letter-spacing="0.8" fill="#0a0a0a">`+"\n", svgFont)
	fmt.Fprintf(&buf, `<text x="16" y="22">FARFIELD ART · DAY %d</text>`+"\n", n)
	fmt.Fprintf(&buf, `<text x="16" y="34" opacity="0.7">%s · CELL [%d %d %d %d] · ORDER 4</text>`+"\n",
		date, c[0], c[1], c[2], c[3])
	fmt.Fprintf(&buf, `<text x="16" y="46" opacity="0.7">BIOME %02d · %s</text>`+"\n",
		biomeIdx, strings.ToUpper(b.Name))
	buf.WriteString("</g>\n")

	// Terrain glyphs, row-major — a fixed order, so the bytes are stable.
	levels := len(b.Ramp)
	fmt.Fprintf(&buf, `<g font-family="%s" font-size="11" text-anchor="middle">`+"\n", svgFont)
	for row := 0; row < plotSize; row++ {
		for col := 0; col < plotSize; col++ {
			lv := hf[row][col]
			x := plotOX + (col-row)*plotDX
			y := plotOY + (col+row)*plotDY - lv*plotDZ
			ink := b.Palette[lv*len(b.Palette)/levels]
			fmt.Fprintf(&buf, `<text x="%d" y="%d" fill="%s">%s</text>`+"\n",
				x, y, ink, string(b.Ramp[lv]))
		}
	}
	buf.WriteString("</g>\n</svg>\n")
	return buf.Bytes()
}

// Structure projection — one 16×16×16 w-slice of the hyperstructure, drawn
// as unit cubes: x' = (x−y)·dx, y' = (x+y)·dy − z·dz.
const (
	structDX = 6
	structDY = 3
	structDZ = 6
	structOX = 200
	structOY = 160
)

// cellStatus classifies one lattice cell of the hyperstructure against the
// current day index.
type cellStatus int

const (
	cellFuture cellStatus = iota // not yet reached — ghosted
	cellFilled                   // accreted on a past day
	cellToday                    // accreted today — accented
)

// statusAt returns the status of cell (x,y,z,w) when day todayN is current:
// its Hilbert day-index against todayN.
func statusAt(x, y, z, w int, todayN uint64) cellStatus {
	idx := hilbert4dInverse(x, y, z, w, artOrder)
	switch {
	case idx == todayN:
		return cellToday
	case idx < todayN:
		return cellFilled
	default:
		return cellFuture
	}
}

// structCell is one occupied cell of a w-slice, ready to draw.
type structCell struct {
	x, y, z int
	biome   int
	today   bool
}

// renderStructureSVG draws the w-slice: every cell the curve has visited as a
// small filled isometric block in its biome's inks, today's cell stroked in
// the accent. The 4,096 future cells are ghosted as one faint base lattice
// (34 lines) rather than 4k individual outlines. Three anchors keep the
// floating clusters legible: the full 16³ bounding box in hairline (front
// edges slightly stronger), a faint footprint square on the floor under every
// occupied (x,y) column, and a dotted drop-line from each column's lowest
// cell to its footprint.
func renderStructureSVG(wSlice int, todayN uint64) []byte {
	// Collect occupied cells, then sort back-to-front, bottom-to-top —
	// painter's order for this projection, and a fixed byte order. The
	// collection order (x, then y, then z ascending) means the first cell
	// seen per (x,y) column is its lowest — the drop-line anchor.
	type footprint struct{ x, y, zmin int }
	var cells []structCell
	var feet []footprint
	for x := 0; x < artSide; x++ {
		for y := 0; y < artSide; y++ {
			colSeen := false
			for z := 0; z < artSide; z++ {
				st := statusAt(x, y, z, wSlice, todayN)
				if st == cellFuture {
					continue
				}
				if !colSeen {
					feet = append(feet, footprint{x: x, y: y, zmin: z})
					colSeen = true
				}
				cells = append(cells, structCell{
					x: x, y: y, z: z,
					biome: biomeIndexAt(x, y, z, wSlice),
					today: st == cellToday,
				})
			}
		}
	}
	sort.Slice(cells, func(i, j int) bool {
		a, b := cells[i], cells[j]
		if a.x+a.y != b.x+b.y {
			return a.x+a.y < b.x+b.y
		}
		if a.z != b.z {
			return a.z < b.z
		}
		return a.x < b.x
	})

	// Lattice corner (i,j) at height k, projected — cells span k ∈ [z, z+1].
	vx := func(i, j int) int { return structOX + (i-j)*structDX }
	vy := func(i, j, k int) int { return structOY + (i+j)*structDY + structDY - k*structDZ }

	var buf bytes.Buffer
	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 400 290" role="img" aria-label="Hyperstructure w-slice %d">`+"\n", wSlice)

	// Title block.
	fmt.Fprintf(&buf, `<g font-family="%s" font-size="9" letter-spacing="0.8" fill="#0a0a0a">`+"\n", svgFont)
	fmt.Fprintf(&buf, `<text x="16" y="22">STRUCTURE · W=%d</text>`+"\n", wSlice)
	fmt.Fprintf(&buf, `<text x="16" y="34" opacity="0.7">DAY %d · %d/%d CELLS</text>`+"\n",
		todayN, len(cells), artSide*artSide*artSide)
	buf.WriteString("</g>\n")

	// Bounding box — all 12 edges of the 16³ volume in hairline, so the
	// slice reads as a volume. The three edges meeting at the front corner
	// are restated stronger after the cells, below.
	edge := func(x1, y1, x2, y2 int) {
		fmt.Fprintf(&buf, `<line x1="%d" y1="%d" x2="%d" y2="%d"/>`+"\n", x1, y1, x2, y2)
	}
	const S = artSide
	buf.WriteString(`<g stroke="#0a0a0a" stroke-opacity="0.15" stroke-width="1">` + "\n")
	for _, k := range [2]int{0, S} { // floor and ceiling rims
		edge(vx(0, 0), vy(0, 0, k), vx(S, 0), vy(S, 0, k))
		edge(vx(S, 0), vy(S, 0, k), vx(S, S), vy(S, S, k))
		edge(vx(S, S), vy(S, S, k), vx(0, S), vy(0, S, k))
		edge(vx(0, S), vy(0, S, k), vx(0, 0), vy(0, 0, k))
	}
	for _, c := range [4][2]int{{0, 0}, {S, 0}, {S, S}, {0, S}} { // verticals
		edge(vx(c[0], c[1]), vy(c[0], c[1], 0), vx(c[0], c[1]), vy(c[0], c[1], S))
	}
	buf.WriteString("</g>\n")

	// Ghost lattice — the base plane the structure accretes onto.
	buf.WriteString(`<g stroke="#0a0a0a" stroke-opacity="0.12" stroke-width="0.5">` + "\n")
	for i := 0; i <= artSide; i++ {
		edge(vx(i, 0), vy(i, 0, 0), vx(i, artSide), vy(i, artSide, 0))
		edge(vx(0, i), vy(0, i, 0), vx(artSide, i), vy(artSide, i, 0))
	}
	buf.WriteString("</g>\n")

	// Footprints — a faint square on the floor under every occupied column,
	// anchoring each cluster to its (x,y) position.
	buf.WriteString(`<g fill="#0a0a0a" fill-opacity="0.1">` + "\n")
	for _, f := range feet {
		fmt.Fprintf(&buf, `<polygon points="%d,%d %d,%d %d,%d %d,%d"/>`+"\n",
			vx(f.x, f.y), vy(f.x, f.y, 0),
			vx(f.x+1, f.y), vy(f.x+1, f.y, 0),
			vx(f.x+1, f.y+1), vy(f.x+1, f.y+1, 0),
			vx(f.x, f.y+1), vy(f.x, f.y+1, 0))
	}
	buf.WriteString("</g>\n")

	// Drop-lines — dotted plumb lines from each column's lowest cell down to
	// its footprint, for columns that float above the floor.
	buf.WriteString(`<g stroke="#0a0a0a" stroke-opacity="0.35" stroke-width="1" stroke-dasharray="1 3">` + "\n")
	for _, f := range feet {
		if f.zmin == 0 {
			continue // resting on the floor — nothing to plumb
		}
		cx := structOX + (f.x-f.y)*structDX
		floorY := structOY + (f.x+f.y)*structDY + structDZ // footprint center
		edge(cx, floorY-f.zmin*structDZ, cx, floorY)
	}
	buf.WriteString("</g>\n")

	// Occupied cells as unit cubes — top, left, right faces in the biome's
	// three inks, lightest up.
	for _, c := range cells {
		cx := structOX + (c.x-c.y)*structDX
		cy := structOY + (c.x+c.y)*structDY - c.z*structDZ
		p := biomes[c.biome].Palette
		stroke := ""
		if c.today {
			stroke = ` stroke="#d93a00" stroke-width="1"`
		}
		// top face
		fmt.Fprintf(&buf, `<polygon points="%d,%d %d,%d %d,%d %d,%d" fill="%s"%s/>`+"\n",
			cx, cy-structDY, cx+structDX, cy, cx, cy+structDY, cx-structDX, cy, p[0], stroke)
		// left face
		fmt.Fprintf(&buf, `<polygon points="%d,%d %d,%d %d,%d %d,%d" fill="%s"%s/>`+"\n",
			cx-structDX, cy, cx, cy+structDY, cx, cy+structDY+structDZ, cx-structDX, cy+structDZ, p[2], stroke)
		// right face
		fmt.Fprintf(&buf, `<polygon points="%d,%d %d,%d %d,%d %d,%d" fill="%s"%s/>`+"\n",
			cx+structDX, cy, cx, cy+structDY, cx, cy+structDY+structDZ, cx+structDX, cy+structDZ, p[1], stroke)
	}

	// Front edges — the three meeting at the near floor corner, restated
	// over the cells slightly stronger so the volume's front reads first.
	buf.WriteString(`<g stroke="#0a0a0a" stroke-opacity="0.4" stroke-width="1">` + "\n")
	edge(vx(S, 0), vy(S, 0, 0), vx(S, S), vy(S, S, 0))
	edge(vx(0, S), vy(0, S, 0), vx(S, S), vy(S, S, 0))
	edge(vx(S, S), vy(S, S, 0), vx(S, S), vy(S, S, S))
	buf.WriteString("</g>\n")

	buf.WriteString("</svg>\n")
	return buf.Bytes()
}
