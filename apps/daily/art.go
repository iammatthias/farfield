package main

import (
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
	svg := renderPlotSVG(date, n, [4]int{x, y, z, w}, bi, b, hf)
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

// renderArtPage renders the plate page for one date. Mirroring the photo
// pages: a past date's page can never change, so it caches for a day with the
// plate CID as ETag; the rolling current day caches for minutes.
func (s *Server) renderArtPage(w http.ResponseWriter, r *http.Request, date string) {
	n, ok := artDayIndex(date)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := artPlotFor(date, n)
	isToday := date == todayUTC()
	if !isToday {
		cacheFor(w, publicMaxAge)
		w.Header().Set("ETag", `"`+p.CID+`"`)
		if web.ETagMatch(r, p.CID) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	} else {
		cacheFor(w, todayMaxAge)
	}
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
		"Nav":          navData(date),
	})
}

// handleArtStructure renders one w-slice of the hyperstructure cube with a
// no-JS scrubber (16 plain links). Default slice: today's w. The view rolls
// forward as cells accrete, so it caches for minutes.
func (s *Server) handleArtStructure(w http.ResponseWriter, r *http.Request) {
	today := todayUTC()
	n, ok := artDayIndex(today)
	if !ok {
		http.NotFound(w, r) // unreachable until ~2199
		return
	}
	x, y, z, tw := hilbert4d(n, artOrder)
	ws := tw
	if q := r.URL.Query().Get("w"); q != "" {
		v, err := strconv.Atoi(q)
		if err != nil || v < 0 || v >= artSide {
			http.NotFound(w, r)
			return
		}
		ws = v
	}
	type slice struct {
		W       int
		URL     string
		Current bool
	}
	slices := make([]slice, artSide)
	for i := range slices {
		slices[i] = slice{W: i, URL: fmt.Sprintf("/art/structure?w=%d", i), Current: i == ws}
	}
	cacheFor(w, todayMaxAge)
	s.rd.Render(w, "art_structure.html", map[string]any{
		"W":      ws,
		"N":      n,
		"Date":   today,
		"Coord":  [4]int{x, y, z, tw},
		"TodayW": tw,
		"OnW":    tw == ws,
		"SVG":    template.HTML(renderStructureSVG(ws, n)),
		"Slices": slices,
	})
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
