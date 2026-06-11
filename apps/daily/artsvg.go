package main

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
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
//
// The terrain's glyph baselines span x ∈ [OX−23·dx, OX+23·dx] and
// y ∈ [OY−(levels−1)·dz, OY+46·dy]; the origin and viewBox below center
// that span with even margins — the plate is pure artwork, its metadata
// lives on the page around it.
const (
	plotSize = 24  // heightfield side
	plotDX   = 10  // half-width of one cell step
	plotDY   = 5   // half-height of one cell step
	plotDZ   = 4   // vertical lift per elevation level
	plotOX   = 250 // isometric origin
	plotOY   = 54
)

// renderPlotSVG draws one day's plate: the heightfield as an isometric
// character-grid terrain, glyph and ink chosen by elevation band. No text
// beyond the terrain glyphs — the artifact is the artwork alone.
func renderPlotSVG(date string, b Biome, hf [][]int) []byte {
	var buf bytes.Buffer
	buf.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 500 310" role="img" aria-label="Generative terrain plate for ` + date + `">` + "\n")

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

// structCell is one occupied cell of a w-slice, ready to draw: its lattice
// position, biome, day index along the curve, and its age band (0 = newest
// ink, structAgeBands-1 = oldest, faintest).
type structCell struct {
	x, y, z int
	biome   int
	idx     uint64
	band    int
	today   bool
}

// Age shading — "ink = elapsed days" made literal. Each cell's age (today's
// index minus the cell's) quantizes into structAgeBands linear bands across
// the whole elapsed span; every ink the cell uses is modulated by its band,
// so the 2020 core fades back while the frontier stays crisp. All opacities
// are fixed strings: no float formatting can leak into the bytes.
const structAgeBands = 5

var (
	// top-face wash — kept faint so the terrain glyphs carry the texture
	structWashOp = [structAgeBands]string{"0.3", "0.24", "0.18", "0.13", "0.09"}
	// side-face base fill, inside the strata patterns
	structSideOp = [structAgeBands]string{"0.88", "0.72", "0.56", "0.42", "0.3"}
	// strata hairlines, inside the patterns
	structLineOp = [structAgeBands]string{"0.9", "0.76", "0.62", "0.5", "0.4"}
	// terrain glyphs on the top faces
	structGlyphOp = [structAgeBands]string{"1", "0.82", "0.66", "0.52", "0.4"}
)

// ageBand quantizes a cell's age into [0, structAgeBands): 0 for today's
// frontier, the top band for the oldest cells.
func ageBand(idx, todayN uint64) int {
	b := int((todayN - idx) * structAgeBands / (todayN + 1))
	if b >= structAgeBands {
		b = structAgeBands - 1
	}
	return b
}

// structTopGrid is the per-axis sample count of the miniature terrain drawn
// on each exposed top face — structTopGrid² glyphs per face. Glyphs stay
// upright at iso-projected sample centers — skewing the miniature into the
// rhombus with a transform was tried and read as a flat weave, losing the
// plate's relief.
const structTopGrid = 4

// q8 formats a coordinate held in eighths of a pixel as a decimal string —
// fixed-point integer math, so the bytes are deterministic everywhere.
// Callers only pass non-negative values.
func q8(n int) string {
	q, r := n>>3, n&7
	if r == 0 {
		return strconv.Itoa(q)
	}
	return strconv.Itoa(q) + "." + strings.TrimRight(strconv.Itoa(r*125), "0")
}

// renderStructureSVG draws the w-slice as an accretion of miniaturized
// day-plates — the same world as /art, at survey scale:
//
//   - Hidden-face culling: only faces against empty (future) space are drawn,
//     so the mass always renders as its exposed shell.
//   - Every exposed top face carries that day's own terrain — the cell's date
//     comes back from the inverse Hilbert mapping, its heightfield is grown
//     exactly as the plate grows it, sampled on a coarse grid, and the biome's
//     ramp glyphs are placed at the projected sample centers in the plate's
//     elevation inks, lifted slightly by elevation. The roof of the structure
//     is terrain.
//   - Exposed side faces are stratified: <pattern> hatching in the biome inks,
//     denser on the left plane than the right so the two planes read apart,
//     like the plate's stacked levels.
//   - Every face is hairline-stroked in the biome's darkest ink, so adjacent
//     same-biome cells never merge into one blob — the mass stays visibly
//     accreted from unit cells.
//   - All inks fade with age (see ageBand): faint = oldest, full = newest,
//     accent outline = today.
//
// The 4,096 future cells are ghosted as one faint base lattice rather than 4k
// outlines. Three anchors keep floating clusters legible: the 16³ bounding
// box in hairline (front edges restated stronger), a faint footprint square
// under every occupied (x,y) column, and a dotted plumb line from each
// column's lowest cell to its footprint.
func renderStructureSVG(wSlice int, todayN uint64) []byte {
	// Collect occupied cells and the occupancy grid, then sort back-to-front,
	// bottom-to-top — painter's order for this projection, and a fixed byte
	// order. The collection order (x, then y, then z ascending) means the
	// first cell seen per (x,y) column is its lowest — the drop-line anchor.
	type footprint struct{ x, y, zmin int }
	var cells []structCell
	var feet []footprint
	var occ [artSide][artSide][artSide]bool
	for x := 0; x < artSide; x++ {
		for y := 0; y < artSide; y++ {
			colSeen := false
			for z := 0; z < artSide; z++ {
				st := statusAt(x, y, z, wSlice, todayN)
				if st == cellFuture {
					continue
				}
				occ[x][y][z] = true
				if !colSeen {
					feet = append(feet, footprint{x: x, y: y, zmin: z})
					colSeen = true
				}
				idx := hilbert4dInverse(x, y, z, wSlice, artOrder)
				cells = append(cells, structCell{
					x: x, y: y, z: z,
					biome: biomeIndexAt(x, y, z, wSlice),
					idx:   idx,
					band:  ageBand(idx, todayN),
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

	// Exposure tests — a face against an occupied neighbor is never visible.
	empty := func(x, y, z int) bool {
		return x >= artSide || y >= artSide || z >= artSide || !occ[x][y][z]
	}
	topExposed := func(c structCell) bool { return empty(c.x, c.y, c.z+1) }
	leftExposed := func(c structCell) bool { return empty(c.x, c.y+1, c.z) }  // the y+1 plane
	rightExposed := func(c structCell) bool { return empty(c.x+1, c.y, c.z) } // the x+1 plane

	// Lattice corner (i,j) at height k, projected — cells span k ∈ [z, z+1].
	vx := func(i, j int) int { return structOX + (i-j)*structDX }
	vy := func(i, j, k int) int { return structOY + (i+j)*structDY + structDY - k*structDZ }

	// The volume's projection spans y ≈ 64..262; the viewBox crops to that
	// span with even margins — no in-image annotation, the page carries the
	// metadata.
	var buf bytes.Buffer
	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 40 400 244" role="img" aria-label="Hyperstructure w-slice %d" font-family="%s">`+"\n", wSlice, svgFont)

	// Strata patterns — one per biome × side × age band actually used, keyed
	// stably as s{biome}{L|R}{band}. The left plane hatches denser (every 2px)
	// than the right (every 3px), so the two side planes of the mass read
	// differently, and the lines run in global pattern space, so strata
	// continue across neighboring cells like beds in a cut bank.
	type patKey struct {
		biome int
		left  bool
		band  int
	}
	used := map[patKey]bool{}
	for _, c := range cells {
		if leftExposed(c) {
			used[patKey{c.biome, true, c.band}] = true
		}
		if rightExposed(c) {
			used[patKey{c.biome, false, c.band}] = true
		}
	}
	keys := make([]patKey, 0, len(used))
	for k := range used {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.biome != b.biome {
			return a.biome < b.biome
		}
		if a.left != b.left {
			return a.left
		}
		return a.band < b.band
	})
	buf.WriteString("<defs>\n")
	for _, k := range keys {
		p := biomes[k.biome].Palette
		side, spacing, base, line := "R", 3, p[1], p[2]
		if k.left {
			side, spacing, base, line = "L", 2, p[2], p[0]
		}
		fmt.Fprintf(&buf, `<pattern id="s%d%s%d" width="4" height="%d" patternUnits="userSpaceOnUse">`, k.biome, side, k.band, spacing)
		fmt.Fprintf(&buf, `<rect width="4" height="%d" fill="%s" fill-opacity="%s"/>`, spacing, base, structSideOp[k.band])
		fmt.Fprintf(&buf, `<line x1="0" y1="0.5" x2="4" y2="0.5" stroke="%s" stroke-opacity="%s" stroke-width="0.5"/>`, line, structLineOp[k.band])
		buf.WriteString("</pattern>\n")
	}
	buf.WriteString("</defs>\n")

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

	// Occupied cells as miniature day-plates, painter's order. Per cell: the
	// exposed faces (top washed in the lightest ink, sides in their strata
	// patterns), each hairline-stroked in the darkest ink so cells never
	// merge; then the day's own terrain glyphs on an exposed top. Fully
	// buried cells draw nothing.
	for _, c := range cells {
		top, left, right := topExposed(c), leftExposed(c), rightExposed(c)
		if !top && !left && !right {
			continue
		}
		cx := structOX + (c.x-c.y)*structDX
		cy := structOY + (c.x+c.y)*structDY - c.z*structDZ
		b := biomes[c.biome]
		p := b.Palette
		stroke := fmt.Sprintf(` stroke="%s" stroke-opacity="0.25" stroke-width="0.5"`, p[2])
		if c.today {
			stroke = ` stroke="#d93a00" stroke-width="1"`
		}
		if top {
			fmt.Fprintf(&buf, `<polygon points="%d,%d %d,%d %d,%d %d,%d" fill="%s" fill-opacity="%s"%s/>`+"\n",
				cx, cy-structDY, cx+structDX, cy, cx, cy+structDY, cx-structDX, cy,
				p[0], structWashOp[c.band], stroke)
		}
		if left {
			fmt.Fprintf(&buf, `<polygon points="%d,%d %d,%d %d,%d %d,%d" fill="url(#s%dL%d)"%s/>`+"\n",
				cx-structDX, cy, cx, cy+structDY, cx, cy+structDY+structDZ, cx-structDX, cy+structDZ,
				c.biome, c.band, stroke)
		}
		if right {
			fmt.Fprintf(&buf, `<polygon points="%d,%d %d,%d %d,%d %d,%d" fill="url(#s%dR%d)"%s/>`+"\n",
				cx+structDX, cy, cx, cy+structDY, cx, cy+structDY+structDZ, cx+structDX, cy+structDZ,
				c.biome, c.band, stroke)
		}
		if !top {
			continue
		}
		// The day's terrain, miniaturized. The cell's date comes back from
		// its day index; its heightfield is the plate's own (same seed, same
		// size, same quantization), sampled structTopGrid² times at the cell
		// centers of a coarse grid. Each sample renders the biome's ramp
		// glyph for its elevation, in the plate's elevation ink, at the
		// iso-projected sample center, lifted half a pixel per level — the
		// plate's relief at 1/20 scale. Coordinates are eighths of a pixel.
		hf := heightfield(newRNG(domainArt, addDays(artifactEpoch, int(c.idx))), b.Terrain, plotSize, len(b.Ramp))
		fmt.Fprintf(&buf, `<g font-size="4" text-anchor="middle" fill-opacity="%s">`+"\n", structGlyphOp[c.band])
		for j := 0; j < structTopGrid; j++ {
			fy8 := 8*c.y + (8*j+4)/structTopGrid
			row := (2*j + 1) * plotSize / (2 * structTopGrid)
			for i := 0; i < structTopGrid; i++ {
				fx8 := 8*c.x + (8*i+4)/structTopGrid
				col := (2*i + 1) * plotSize / (2 * structTopGrid)
				lv := hf[row][col]
				px8 := 8*structOX + (fx8-fy8)*structDX
				py8 := 8*structOY + (fx8+fy8+8)*structDY - 8*(c.z+1)*structDZ - 4*lv + 12
				fmt.Fprintf(&buf, `<text x="%s" y="%s" fill="%s">%s</text>`+"\n",
					q8(px8), q8(py8), p[lv*len(p)/len(b.Ramp)], string(b.Ramp[lv]))
			}
		}
		buf.WriteString("</g>\n")
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
