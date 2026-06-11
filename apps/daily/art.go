package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
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

// artPlot is one fully derived day: cell, biome, rendered plate, identity.
type artPlot struct {
	Date     string
	N        uint64
	Coord    [4]int
	BiomeIdx int
	Biome    Biome
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
		BiomeIdx: bi, Biome: b, SVG: svg, CID: cid.Of(svg),
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
	svgURL, jsonURL := "/art.svg", "/api/art"
	if !isToday {
		svgURL, jsonURL = "/art/"+date+".svg", "/api/art/"+date
	}
	maxAge := todayMaxAge
	if !isToday {
		maxAge = publicMaxAge // a past day's plate never changes
	}
	cacheFor(w, maxAge)
	s.rd.Render(w, "art.html", map[string]any{
		"Date":         p.Date,
		"N":            p.N,
		"Coord":        p.Coord,
		"Biome":        p.Biome.Name,
		"BiomeIdx":     p.BiomeIdx,
		"CID":          p.CID,
		"Epoch":        artifactEpoch,
		"SVG":          template.HTML(p.SVG),
		"SVGURL":       svgURL,
		"JSONURL":      jsonURL,
		"PrevURL":      prevURL,
		"NextURL":      nextURL,
		"StructureURL": fmt.Sprintf("/art/structure?w=%d", p.Coord[3]),
		"Nav":          navData(date, "art"),
	})
}

// handleArtStructure renders one w-slice of the hyperstructure cube with a
// no-JS fill-map scrubber: 16 bars, one per slice, height proportional to
// the cells the curve has accreted there; each bar links to its slice.
// Default slice: today's w.
func (s *Server) handleArtStructure(w http.ResponseWriter, r *http.Request) {
	today := todayUTC()
	n, ok := artDayIndex(today)
	if !ok {
		http.NotFound(w, r) // unreachable until ~2199
		return
	}
	x, y, z, tw := hilbert4d(n, artOrder)
	ws, ok := structureSliceW(r, tw)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Occupied cells per w-slice — walk the curve once, day 0 through today.
	counts := make([]int, artSide)
	maxCount := 0
	for i := uint64(0); i <= n; i++ {
		_, _, _, cw := hilbert4d(i, artOrder)
		counts[cw]++
		if counts[cw] > maxCount {
			maxCount = counts[cw]
		}
	}

	// fillBarMax is the tallest bar of the fill-map, in CSS pixels; the rest
	// scale proportionally, with a 2px floor so a non-empty slice stays visible.
	const fillBarMax = 48
	type slice struct {
		W       int
		URL     string
		Current bool
		Count   int
		BarPx   int
	}
	slices := make([]slice, artSide)
	for i := range slices {
		px := counts[i] * fillBarMax / maxCount
		if counts[i] > 0 && px < 2 {
			px = 2
		}
		slices[i] = slice{
			W: i, URL: fmt.Sprintf("/art/structure?w=%d", i),
			Current: i == ws, Count: counts[i], BarPx: px,
		}
	}
	cacheFor(w, todayMaxAge)
	s.rd.Render(w, "art_structure.html", map[string]any{
		"W":          ws,
		"N":          n,
		"Date":       today,
		"Coord":      [4]int{x, y, z, tw},
		"TodayW":     tw,
		"OnW":        tw == ws,
		"Count":      counts[ws],
		"SliceCells": artSide * artSide * artSide,
		"SVG":        template.HTML(renderStructureSVG(ws, n)),
		"Slices":     slices,
		"JSVer":      structureJSVer,
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

//go:embed static/vendor/three.module.min.js
var threeJS []byte

//go:embed static/vendor/OrbitControls.js
var orbitControlsJS []byte

var (
	structureJSVer = cid.Of(structureJS)[:16]
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

// ── structure JSON API ─────────────────────────────────────────────────────

// structureSliceW resolves the ?w= query — defaulting to today's slice — and
// reports ok=false when it is out of range.
func structureSliceW(r *http.Request, todayW int) (int, bool) {
	q := r.URL.Query().Get("w")
	if q == "" {
		return todayW, true
	}
	v, err := strconv.Atoi(q)
	if err != nil || v < 0 || v >= artSide {
		return 0, false
	}
	return v, true
}

// handleAPIArtStructure emits one w-slice of the hyperstructure as JSON — the
// occupied cells of that slice in day order, plus the biome palette table —
// for the three.js viewer. Like /api/art it rolls forward at midnight UTC, so
// it is publicly cacheable for minutes, with its content CID as ETag.
func (s *Server) handleAPIArtStructure(w http.ResponseWriter, r *http.Request) {
	today := todayUTC()
	n, ok := artDayIndex(today)
	if !ok {
		web.WriteError(w, http.StatusNotFound, "no structure today") // unreachable until ~2199
		return
	}
	tx, ty, tz, tw := hilbert4d(n, artOrder)
	ws, ok := structureSliceW(r, tw)
	if !ok {
		web.WriteError(w, http.StatusNotFound, "no such slice — w must be 0..15")
		return
	}

	// Walk the curve once, day 0 through today; keep this slice's cells.
	// The walk is in day order, so the cells arrive pre-sorted for the
	// viewer's accretion animation.
	type apiCell struct {
		I     uint64 `json:"i"`
		X     int    `json:"x"`
		Y     int    `json:"y"`
		Z     int    `json:"z"`
		Biome int    `json:"biome"`
		Age   int    `json:"age"` // age band, 0 = newest ink … bands-1 = oldest
		Today bool   `json:"today"`
	}
	cells := make([]apiCell, 0, n/artSide+1)
	for i := uint64(0); i <= n; i++ {
		x, y, z, cw := hilbert4d(i, artOrder)
		if cw != ws {
			continue
		}
		cells = append(cells, apiCell{
			I: i, X: x, Y: y, Z: z,
			Biome: biomeIndexAt(x, y, z, ws),
			Age:   ageBand(i, n),
			Today: i == n,
		})
	}
	type apiBiome struct {
		Name   string   `json:"name"`
		Colors []string `json:"colors"`
	}
	bs := make([]apiBiome, len(biomes))
	for i, b := range biomes {
		bs[i] = apiBiome{Name: b.Name, Colors: b.Palette[:]}
	}
	body, err := json.Marshal(map[string]any{
		"w":      ws,
		"side":   artSide,
		"date":   today,
		"epoch":  artifactEpoch,
		"bands":  structAgeBands,
		"today":  map[string]any{"index": n, "coord": []int{tx, ty, tz, tw}},
		"cells":  cells,
		"biomes": bs,
	})
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
