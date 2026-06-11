package main

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/web"
)

// The art artifact — a 4-D hyperstructure accreting one cell per day. Each
// date is a day index N along a 4-D Hilbert curve of order 4 (16⁴ = 65,536
// cells, ~179 years from the 2020-01-01 epoch). The cell's neighborhood picks
// a biome; a ChaCha8 stream seeded from sha256("art:"+date) grows a value-
// noise terrain in that biome's style; the plate is rendered to a
// byte-deterministic SVG. Nothing is stored — every plate derives purely from
// its date, so its CID is its permanent identity.

const (
	artOrder = 4                   // Hilbert curve order
	artSide  = 1 << artOrder       // lattice side per axis (16)
	artCells = 1 << (4 * artOrder) // total cells (65,536)
)

// artDayIndex returns the hyperstructure day index for a date, or ok=false
// when the date is malformed, pre-epoch, in the future, or past the curve's
// end (~year 2199).
func artDayIndex(date string) (uint64, bool) {
	if !validDate(date) || date > todayUTC() {
		return 0, false
	}
	n, err := dayIndex(date)
	if err != nil || n < 0 || n >= artCells {
		return 0, false
	}
	return uint64(n), true
}

// artPlot is one fully derived day: cell, biome, heightfield, rendered
// plate, identity.
type artPlot struct {
	Date     string
	N        uint64
	Coord    [4]int
	BiomeIdx int
	Biome    Biome
	HF       [][]int
	SVG      []byte
	CID      string
}

// artPlotFor derives the complete plot for a day index — pure, no storage.
func artPlotFor(date string, n uint64) artPlot {
	x, y, z, w := hilbert4d(n, artOrder)
	bi := biomeIndexAt(x, y, z, w)
	b := biomes[bi]
	hf := heightfield(newRNG(domainArt, date), b.Terrain, plotSize, len(b.Ramp))
	svg := renderPlotSVG(date, b, hf)
	return artPlot{
		Date: date, N: n, Coord: [4]int{x, y, z, w},
		BiomeIdx: bi, Biome: b, HF: hf, SVG: svg, CID: cid.Of(svg),
	}
}

// ── HTML handlers ──────────────────────────────────────────────────────────

// handleArtToday renders today's plate page.
func (s *Server) handleArtToday(w http.ResponseWriter, r *http.Request) {
	s.renderArtPage(w, r, todayUTC())
}

// handleArtDay renders one date's plate page; a ".svg" suffix serves the raw
// plate instead (ServeMux wildcards span whole segments, so /art/{date}.svg
// arrives here).
func (s *Server) handleArtDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if d, ok := strings.CutSuffix(date, ".svg"); ok {
		s.writeArtSVG(w, r, d)
		return
	}
	s.renderArtPage(w, r, date)
}

// renderArtPage renders the plate page for one date. Nothing on the page
// varies per visitor, so the HTML is publicly cacheable — a past day's page
// for a day (its plate is immutable), today's for minutes.
func (s *Server) renderArtPage(w http.ResponseWriter, r *http.Request, date string) {
	n, ok := artDayIndex(date)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := artPlotFor(date, n)
	isToday := date == todayUTC()
	prevURL, nextURL := "", ""
	if date > artifactEpoch {
		prevURL = "/art/" + addDays(date, -1)
	}
	if !isToday {
		nextURL = "/art/" + addDays(date, 1)
	}
	svgURL, jsonURL, terrainURL := "/art.svg", "/api/art", "/api/art/terrain"
	if !isToday {
		svgURL, jsonURL = "/art/"+date+".svg", "/api/art/"+date
		terrainURL = "/api/art/terrain/" + date
	}
	maxAge := todayMaxAge
	if !isToday {
		maxAge = publicMaxAge // a past day's plate never changes
	}
	cacheFor(w, maxAge)
	s.rd.Render(w, "art.html", map[string]any{
		"Date":       p.Date,
		"N":          p.N,
		"Biome":      p.Biome.Name,
		"SVG":        template.HTML(p.SVG),
		"SVGURL":     svgURL,
		"JSONURL":    jsonURL,
		"TerrainURL": terrainURL,
		"PrevURL":    prevURL,
		"NextURL":    nextURL,
		"ArtJSVer":   artJSVer,
		"TerrainVer": terrainJSVer,
		"ThreeVer":   threeJSVer,
		"OrbitVer":   orbitJSVer,
		"Nav":        navData(date, "art"),
	})
}

// handleArtStructure renders the structure page: the whole accreted
// hyperstructure as one scene, no apparatus. The server renders today's
// slice as the SVG fallback; structure.js swaps in a three.js scene of
// every occupied cell across all slices.
func (s *Server) handleArtStructure(w http.ResponseWriter, r *http.Request) {
	today := todayUTC()
	n, ok := artDayIndex(today)
	if !ok {
		http.NotFound(w, r) // unreachable until ~2199
		return
	}
	_, _, _, tw := hilbert4d(n, artOrder)
	cacheFor(w, todayMaxAge)
	s.rd.Render(w, "art_structure.html", map[string]any{
		"N":          n,
		"Date":       today,
		"Epoch":      artifactEpoch,
		"SVG":        template.HTML(renderStructureSVG(tw, n)),
		"JSVer":      structureJSVer,
		"TerrainVer": terrainJSVer,
		"ThreeVer":   threeJSVer,
		"OrbitVer":   orbitJSVer,
		"Nav":        navData(today, "art"),
	})
}

// ── structure viewer assets ────────────────────────────────────────────────
//
// The structure page progressively enhances its server-rendered SVG with a
// three.js scene. All scripts are app-local and embedded: the vendored
// three.js r170 module + OrbitControls (MIT — see static/vendor/README.md)
// and the scene script. Each is fingerprinted with its content CID and served
// immutable, like sudoku.js/wordle.js; an importmap in the template maps the
// bare specifier "three" to the vendored module, so no CDN and no build step.

//go:embed static/structure.js
var structureJS []byte

//go:embed static/art.js
var artJS []byte

//go:embed static/terrain.js
var terrainJS []byte

//go:embed static/vendor/three.module.min.js
var threeJS []byte

//go:embed static/vendor/OrbitControls.js
var orbitControlsJS []byte

var (
	structureJSVer = cid.Of(structureJS)[:16]
	artJSVer       = cid.Of(artJS)[:16]
	terrainJSVer   = cid.Of(terrainJS)[:16]
	threeJSVer     = cid.Of(threeJS)[:16]
	orbitJSVer     = cid.Of(orbitControlsJS)[:16]
)

// immutableJSHandler serves an embedded script with immutable caching and its
// CID as a strong ETag — the URL carries the version, so the bytes never
// change under it. Gzip comes from the app-wide middleware.
func immutableJSHandler(body []byte, etag string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/javascript; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("ETag", `"`+etag+`"`)
		if web.ETagMatch(r, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(body)
	}
}

// ── shared terrain language ────────────────────────────────────────────────
//
// Both art pages draw the same geometry — a quantized heightfield as
// terraced terrain — at two sample densities. The plate page gets the full
// 24×24 field via /api/art/terrain; each structure cell carries a miniature
// of its own day's field, downsampled at the same cell centers the SVG
// miniatures sample.

// structTileGrid is the per-axis sample count of the miniature tile each
// structure cell carries; today's cell ships denser, near plate resolution.
const (
	structTileGrid      = 6
	structTodayTileGrid = 12
)

// tileLevels downsamples a plate heightfield onto a g×g grid, row-major,
// sampling at cell centers — the same centers renderStructureSVG uses for
// its glyph miniatures, so the viewer's tiles and the SVG's tops agree.
func tileLevels(hf [][]int, g int) []int {
	out := make([]int, 0, g*g)
	for j := 0; j < g; j++ {
		row := (2*j + 1) * plotSize / (2 * g)
		for i := 0; i < g; i++ {
			col := (2*i + 1) * plotSize / (2 * g)
			out = append(out, hf[row][col])
		}
	}
	return out
}

// handleAPIArtTerrainToday emits today's heightfield for the plate viewer.
func (s *Server) handleAPIArtTerrainToday(w http.ResponseWriter, r *http.Request) {
	s.writeArtTerrainJSON(w, r, todayUTC(), todayMaxAge)
}

// handleAPIArtTerrainDay emits one date's heightfield.
func (s *Server) handleAPIArtTerrainDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	maxAge := publicMaxAge
	if date == todayUTC() {
		maxAge = todayMaxAge
	}
	s.writeArtTerrainJSON(w, r, date, maxAge)
}

// writeArtTerrainJSON emits one day's quantized heightfield — the plate's
// server-canonical terrain, so the client never re-derives noise — plus the
// biome inks to draw it in. Cached like /api/art, with the plate CID as
// ETag: the levels and the SVG derive from the same stream, so the CID
// versions both.
func (s *Server) writeArtTerrainJSON(w http.ResponseWriter, r *http.Request, date string, maxAge int) {
	n, ok := artDayIndex(date)
	if !ok {
		web.WriteError(w, http.StatusNotFound, "no art for that date")
		return
	}
	p := artPlotFor(date, n)
	levels := make([]int, 0, plotSize*plotSize)
	for _, row := range p.HF {
		levels = append(levels, row...)
	}
	cacheFor(w, maxAge)
	w.Header().Set("ETag", `"`+p.CID+`"`)
	if web.ETagMatch(r, p.CID) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"date":  p.Date,
		"n":     p.N,
		"side":  plotSize,
		"bands": len(p.Biome.Ramp),
		"biome": map[string]any{
			"index":  p.BiomeIdx,
			"name":   p.Biome.Name,
			"colors": p.Biome.Palette[:],
		},
		"levels": levels,
		"cid":    p.CID,
	})
}

// ── structure JSON API ─────────────────────────────────────────────────────

// structAPICell is one occupied cell as the structure API emits it.
type structAPICell struct {
	I     uint64 `json:"i"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	Z     int    `json:"z"`
	Biome int    `json:"biome"`
	Age   int    `json:"age"` // age band, 0 = newest ink … bands-1 = oldest
	Today bool   `json:"today"`
	HF    []int  `json:"hf"` // tile levels, row-major, side = √len; empty = buried
}

// structAPISlice is one non-empty w-slice of the full-structure response,
// its cells in day order.
type structAPISlice struct {
	W     int             `json:"w"`
	Cells []structAPICell `json:"cells"`
}

// structCellAt derives one cell of the structure response. The heightfield
// is the cell's own day's, downsampled to a miniature tile (today's at
// higher density), so the viewer renders real terrain, never re-derived
// noise.
func structCellAt(i, n uint64, x, y, z, w int) structAPICell {
	bi := biomeIndexAt(x, y, z, w)
	b := biomes[bi]
	grid := structTileGrid
	if i == n {
		grid = structTodayTileGrid
	}
	hf := heightfield(newRNG(domainArt, addDays(artifactEpoch, int(i))), b.Terrain, plotSize, len(b.Ramp))
	return structAPICell{
		I: i, X: x, Y: y, Z: z,
		Biome: bi,
		Age:   ageBand(i, n),
		Today: i == n,
		HF:    tileLevels(hf, grid),
	}
}

// structureSliceCells walks the curve day 0 through n and returns one
// w-slice's occupied cells in day order — pre-sorted for the viewer's
// accretion animation.
func structureSliceCells(n uint64, ws int) []structAPICell {
	cells := make([]structAPICell, 0, n/artSide+1)
	for i := uint64(0); i <= n; i++ {
		x, y, z, cw := hilbert4d(i, artOrder)
		if cw != ws {
			continue
		}
		cells = append(cells, structCellAt(i, n, x, y, z, cw))
	}
	return cells
}

// structureSlices walks the curve day 0 through n and returns every
// non-empty w-slice, each slice's cells in day order. A fully buried cell
// (all six neighbors within its slice occupied) renders nothing in the
// viewer, so its heightfield tile is trimmed to empty — exposed cells keep
// theirs — which keeps the full payload compact.
func structureSlices(n uint64) []structAPISlice {
	var occ [artSide][artSide][artSide][artSide]bool // [w][x][y][z]
	for i := uint64(0); i <= n; i++ {
		x, y, z, w := hilbert4d(i, artOrder)
		occ[w][x][y][z] = true
	}
	at := func(w, x, y, z int) bool {
		if x < 0 || y < 0 || z < 0 || x >= artSide || y >= artSide || z >= artSide {
			return false // off the lattice = exposed
		}
		return occ[w][x][y][z]
	}
	buried := func(w, x, y, z int) bool {
		return at(w, x+1, y, z) && at(w, x-1, y, z) &&
			at(w, x, y+1, z) && at(w, x, y-1, z) &&
			at(w, x, y, z+1) && at(w, x, y, z-1)
	}
	byW := make([][]structAPICell, artSide)
	for i := uint64(0); i <= n; i++ {
		x, y, z, w := hilbert4d(i, artOrder)
		c := structCellAt(i, n, x, y, z, w)
		if i != n && buried(w, x, y, z) {
			c.HF = []int{} // invisible — no tile needed
		}
		byW[w] = append(byW[w], c)
	}
	slices := make([]structAPISlice, 0, artSide)
	for w, cells := range byW {
		if len(cells) > 0 {
			slices = append(slices, structAPISlice{W: w, Cells: cells})
		}
	}
	return slices
}

// handleAPIArtStructure emits the hyperstructure as JSON for the three.js
// viewer. Without ?w it returns the full structure — every non-empty
// w-slice, each slice's occupied cells in day order — plus the biome
// palette table. With ?w=N it returns that single slice in the original
// flat shape (back-compat; every cell keeps its tile there). Like /api/art
// it rolls forward at midnight UTC, so it is publicly cacheable for
// minutes, with its content CID as ETag.
func (s *Server) handleAPIArtStructure(w http.ResponseWriter, r *http.Request) {
	today := todayUTC()
	n, ok := artDayIndex(today)
	if !ok {
		web.WriteError(w, http.StatusNotFound, "no structure today") // unreachable until ~2199
		return
	}
	tx, ty, tz, tw := hilbert4d(n, artOrder)

	payload := map[string]any{
		"side":    artSide,
		"date":    today,
		"epoch":   artifactEpoch,
		"bands":   structAgeBands,
		"hfBands": len(biomes[0].Ramp), // every ramp quantizes to the same level count
		"today":   map[string]any{"index": n, "coord": []int{tx, ty, tz, tw}},
	}
	if q := r.URL.Query().Get("w"); q != "" {
		ws, err := strconv.Atoi(q)
		if err != nil || ws < 0 || ws >= artSide {
			web.WriteError(w, http.StatusNotFound, "no such slice — w must be 0..15")
			return
		}
		payload["w"] = ws
		payload["cells"] = structureSliceCells(n, ws)
	} else {
		payload["slices"] = structureSlices(n)
	}
	type apiBiome struct {
		Name   string   `json:"name"`
		Colors []string `json:"colors"`
	}
	bs := make([]apiBiome, len(biomes))
	for i, b := range biomes {
		bs[i] = apiBiome{Name: b.Name, Colors: b.Palette[:]}
	}
	payload["biomes"] = bs

	body, err := json.Marshal(payload)
	if err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not encode structure")
		return
	}
	etag := cid.Of(body)
	cacheFor(w, todayMaxAge)
	w.Header().Set("ETag", `"`+etag+`"`)
	if web.ETagMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

// ── SVG handlers ───────────────────────────────────────────────────────────

// handleArtSVGToday serves today's raw plate.
func (s *Server) handleArtSVGToday(w http.ResponseWriter, r *http.Request) {
	s.writeArtSVG(w, r, todayUTC())
}

// writeArtSVG serves one date's plate as image/svg+xml with its CID as a
// strong ETag. A past date's plate is a pure function of the date — its bytes
// can never change — so it is immutable; today's caches for minutes.
func (s *Server) writeArtSVG(w http.ResponseWriter, r *http.Request, date string) {
	n, ok := artDayIndex(date)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := artPlotFor(date, n)
	if date < todayUTC() {
		cacheImmutable(w)
	} else {
		cacheFor(w, todayMaxAge)
	}
	w.Header().Set("ETag", `"`+p.CID+`"`)
	if web.ETagMatch(r, p.CID) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	_, _ = w.Write(p.SVG)
}

// ── JSON API handlers ──────────────────────────────────────────────────────

func (s *Server) handleAPIArtToday(w http.ResponseWriter, r *http.Request) {
	s.writeArtJSON(w, r, todayUTC(), todayMaxAge)
}

func (s *Server) handleAPIArtDay(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	maxAge := publicMaxAge
	if date == todayUTC() {
		maxAge = todayMaxAge
	}
	s.writeArtJSON(w, r, date, maxAge)
}

// writeArtJSON emits one day's derivation — date, cell, biome, and the CID of
// its plate SVG — with the CID as ETag.
func (s *Server) writeArtJSON(w http.ResponseWriter, r *http.Request, date string, maxAge int) {
	n, ok := artDayIndex(date)
	if !ok {
		web.WriteError(w, http.StatusNotFound, "no art for that date")
		return
	}
	p := artPlotFor(date, n)
	cacheFor(w, maxAge)
	w.Header().Set("ETag", `"`+p.CID+`"`)
	if web.ETagMatch(r, p.CID) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	web.WriteJSON(w, http.StatusOK, map[string]any{
		"date":  p.Date,
		"coord": p.Coord[:],
		"biome": p.Biome.Name,
		"cid":   p.CID,
	})
}

// cacheImmutable marks a response permanently cacheable — for derived content
// that can never change, like a past date's deterministic SVG.
func cacheImmutable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
}
