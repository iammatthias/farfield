package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iammatthias/farfield/lib/theme"
	"github.com/iammatthias/farfield/lib/web"
)

func TestSeedDeterminismAndDomainSeparation(t *testing.T) {
	if seed("art", "2024-03-15") != seed("art", "2024-03-15") {
		t.Error("the same domain and date must yield the same seed")
	}
	if seed("art", "2024-03-15") == seed("sudoku", "2024-03-15") {
		t.Error("different domains must yield different seeds for one date")
	}
	if seed("art", "2024-03-15") == seed("art", "2024-03-16") {
		t.Error("different dates must yield different seeds for one domain")
	}
}

func TestDayIndex(t *testing.T) {
	if n, err := dayIndex(artifactEpoch); err != nil || n != 0 {
		t.Errorf("epoch day index = %d, err %v, want 0", n, err)
	}
	if n, err := dayIndex("2020-01-02"); err != nil || n != 1 {
		t.Errorf("epoch+1 day index = %d, err %v, want 1", n, err)
	}
	if n, err := dayIndex("2019-12-31"); err != nil || n != -1 {
		t.Errorf("pre-epoch day index = %d, err %v, want -1", n, err)
	}
}

func TestArtDayIndexBounds(t *testing.T) {
	if _, ok := artDayIndex("2019-12-31"); ok {
		t.Error("pre-epoch dates must not exist")
	}
	if _, ok := artDayIndex(addDays(todayUTC(), 1)); ok {
		t.Error("future dates must not exist")
	}
	if _, ok := artDayIndex("not-a-date"); ok {
		t.Error("malformed dates must not exist")
	}
	if n, ok := artDayIndex(artifactEpoch); !ok || n != 0 {
		t.Errorf("epoch = (%d, %v), want (0, true)", n, ok)
	}
	if n, ok := artDayIndex(todayUTC()); !ok || n == 0 {
		t.Errorf("today = (%d, %v), want a positive index", n, ok)
	}
}

func TestHeightfieldDeterministicAndInRange(t *testing.T) {
	p := biomes[0].Terrain
	a := heightfield(newRNG(domainArt, "2024-03-15"), p, plotSize, 6)
	b := heightfield(newRNG(domainArt, "2024-03-15"), p, plotSize, 6)
	for r := range a {
		for c := range a[r] {
			if a[r][c] != b[r][c] {
				t.Fatalf("heightfield differs at (%d,%d): %d vs %d", r, c, a[r][c], b[r][c])
			}
			if a[r][c] < 0 || a[r][c] >= 6 {
				t.Fatalf("level %d at (%d,%d) out of [0,6)", a[r][c], r, c)
			}
		}
	}
}

// TestPlotSVGDeterministic: the same date must render byte-identical SVG —
// and therefore the same CID — every time; different dates must differ.
func TestPlotSVGDeterministic(t *testing.T) {
	n, ok := artDayIndex("2024-03-15")
	if !ok {
		t.Fatal("2024-03-15 should be a valid art date")
	}
	a := artPlotFor("2024-03-15", n)
	b := artPlotFor("2024-03-15", n)
	if !bytes.Equal(a.SVG, b.SVG) {
		t.Error("the same date must render byte-identical SVG")
	}
	if a.CID == "" || a.CID != b.CID {
		t.Errorf("the same date must yield the same CID, got %q and %q", a.CID, b.CID)
	}
	m, _ := artDayIndex("2024-03-16")
	c := artPlotFor("2024-03-16", m)
	if c.CID == a.CID {
		t.Error("different dates must yield different CIDs")
	}
}

// TestStructureCellStatus: a cell the curve has passed is filled, today's
// cell is marked, a cell it has not reached is future.
func TestStructureCellStatus(t *testing.T) {
	const todayN = uint64(1000)
	x, y, z, w := hilbert4d(500, artOrder)
	if got := statusAt(x, y, z, w, todayN); got != cellFilled {
		t.Errorf("past cell status = %d, want filled", got)
	}
	x, y, z, w = hilbert4d(todayN, artOrder)
	if got := statusAt(x, y, z, w, todayN); got != cellToday {
		t.Errorf("today cell status = %d, want today", got)
	}
	x, y, z, w = hilbert4d(todayN+1, artOrder)
	if got := statusAt(x, y, z, w, todayN); got != cellFuture {
		t.Errorf("future cell status = %d, want future", got)
	}
}

func TestStructureSVGDeterministic(t *testing.T) {
	a := renderStructureSVG(3, 1000)
	b := renderStructureSVG(3, 1000)
	if !bytes.Equal(a, b) {
		t.Error("the same slice and day must render byte-identical SVG")
	}
	if bytes.Equal(a, renderStructureSVG(4, 1000)) {
		t.Error("different slices must render different SVG")
	}
}

// testServer builds a Server with templates parsed and a throwaway database,
// enough to exercise the art handlers end to end.
func testServer(t *testing.T) *Server {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "art.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	tmpl, err := web.ParseTemplates(assets, templateFuncs)
	if err != nil {
		t.Fatalf("templates: %v", err)
	}
	return &Server{
		db:      db,
		fetcher: newFetcher("DEMO_KEY"),
		rd:      &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
	}
}

func TestArtEndpoints(t *testing.T) {
	s := testServer(t)
	h := s.routes()

	get := func(path, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		rec := httptest.NewRecorder()
		req.Header.Set("Accept-Encoding", "identity")
		h.ServeHTTP(rec, req)
		return rec
	}

	// Today's page is HTML with the plate inline.
	if rec := get("/art", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "<svg") {
		t.Errorf("/art = %d, body contains svg: %v", rec.Code, strings.Contains(rec.Body.String(), "<svg"))
	}

	// A past date's SVG: stable bytes, immutable, CID ETag, 304 on revalidate.
	first := get("/art/2024-03-15.svg", "")
	if first.Code != 200 {
		t.Fatalf("/art/2024-03-15.svg = %d", first.Code)
	}
	if ct := first.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Errorf("svg content type = %q", ct)
	}
	if cc := first.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("past svg cache-control = %q, want immutable", cc)
	}
	second := get("/art/2024-03-15.svg", "")
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Error("the same past date must serve byte-identical SVG")
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("svg response must carry an ETag")
	}
	if rec := get("/art/2024-03-15.svg", etag); rec.Code != 304 {
		t.Errorf("revalidation = %d, want 304", rec.Code)
	}

	// Structure slices render for any w in range; out of range is 404.
	if rec := get("/art/structure?w=3", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "<svg") {
		t.Errorf("/art/structure?w=3 = %d", rec.Code)
	}
	if rec := get("/art/structure?w=16", ""); rec.Code != 404 {
		t.Errorf("/art/structure?w=16 = %d, want 404", rec.Code)
	}

	// JSON carries coord and cid.
	if rec := get("/api/art", ""); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `"coord"`) || !strings.Contains(rec.Body.String(), `"cid"`) {
		t.Errorf("/api/art = %d body %s", rec.Code, rec.Body.String())
	}

	// The plate is pure artwork — no annotation text baked into the bytes.
	// Its metadata lives on the page instead.
	if body := first.Body.String(); strings.Contains(body, "FARFIELD ART") || strings.Contains(body, "<text x=\"16\"") {
		t.Error("plot SVG must not embed a title block")
	}

	// Out-of-archive dates do not exist.
	for _, path := range []string{"/art/2019-12-31", "/art/2019-12-31.svg",
		"/art/" + addDays(todayUTC(), 1), "/api/art/2019-12-31"} {
		if rec := get(path, ""); rec.Code != 404 {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}

// TestArtStructureAPI: the structure JSON carries the slice's occupied cells
// in day order plus the biome table, caches like the other today-rolling
// reads, and 404s for slices off the lattice.
func TestArtStructureAPI(t *testing.T) {
	s := testServer(t)
	h := s.routes()

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept-Encoding", "identity")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get("/api/art/structure?w=6")
	if rec.Code != 200 {
		t.Fatalf("/api/art/structure?w=6 = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "public") {
		t.Errorf("cache-control = %q, want public", cc)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("structure JSON must carry an ETag")
	}
	var body struct {
		W     int    `json:"w"`
		Side  int    `json:"side"`
		Bands int    `json:"bands"`
		Date  string `json:"date"`
		Today struct {
			Index uint64 `json:"index"`
			Coord []int  `json:"coord"`
		} `json:"today"`
		Cells []struct {
			I     uint64 `json:"i"`
			X     int    `json:"x"`
			Y     int    `json:"y"`
			Z     int    `json:"z"`
			Biome int    `json:"biome"`
			Age   int    `json:"age"`
			Today bool   `json:"today"`
		} `json:"cells"`
		Biomes []struct {
			Name   string   `json:"name"`
			Colors []string `json:"colors"`
		} `json:"biomes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.W != 6 || body.Side != artSide || body.Bands != structAgeBands {
		t.Errorf("w/side/bands = %d/%d/%d", body.W, body.Side, body.Bands)
	}
	if len(body.Today.Coord) != 4 || body.Today.Index == 0 {
		t.Errorf("today = %+v, want index>0 and a 4-coord", body.Today)
	}
	if len(body.Biomes) != len(biomes) {
		t.Fatalf("biomes = %d, want %d", len(body.Biomes), len(biomes))
	}
	for i, b := range body.Biomes {
		if b.Name != biomes[i].Name || len(b.Colors) != 3 {
			t.Errorf("biome %d = %+v", i, b)
		}
	}
	prev := int64(-1)
	for _, c := range body.Cells {
		if c.X < 0 || c.X >= artSide || c.Y < 0 || c.Y >= artSide || c.Z < 0 || c.Z >= artSide {
			t.Fatalf("cell %d off the lattice: %+v", c.I, c)
		}
		if c.Biome < 0 || c.Biome >= len(biomes) || c.Age < 0 || c.Age >= structAgeBands {
			t.Fatalf("cell %d bad biome/age: %+v", c.I, c)
		}
		if int64(c.I) <= prev {
			t.Fatal("cells must arrive in ascending day order")
		}
		prev = int64(c.I)
		if c.Today && c.I != body.Today.Index {
			t.Errorf("today cell index %d != %d", c.I, body.Today.Index)
		}
	}
	// The today flag appears exactly when today's w is this slice.
	wantToday := body.Today.Coord[3] == 6
	gotToday := false
	for _, c := range body.Cells {
		gotToday = gotToday || c.Today
	}
	if gotToday != wantToday {
		t.Errorf("today flag present = %v, want %v", gotToday, wantToday)
	}

	// Off-lattice slices do not exist.
	for _, path := range []string{"/api/art/structure?w=16", "/api/art/structure?w=-1",
		"/api/art/structure?w=abc"} {
		if rec := get(path); rec.Code != 404 {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}

	// No ?w defaults to today's slice.
	rec = get("/api/art/structure")
	if rec.Code != 200 {
		t.Fatalf("/api/art/structure = %d", rec.Code)
	}
}

// TestStructureViewerAssets: the vendored three.js modules and the scene
// script serve immutable with CID ETags, and the page wires them up via the
// importmap and versioned URLs.
func TestStructureViewerAssets(t *testing.T) {
	s := testServer(t)
	h := s.routes()

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept-Encoding", "identity")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for path, ver := range map[string]string{
		"/static/structure.js":               structureJSVer,
		"/static/vendor/three.module.min.js": threeJSVer,
		"/static/vendor/OrbitControls.js":    orbitJSVer,
	} {
		rec := get(path + "?v=" + ver)
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s cache-control = %q, want immutable", path, cc)
		}
		if rec.Header().Get("ETag") != `"`+ver+`"` {
			t.Errorf("%s etag = %q, want %q", path, rec.Header().Get("ETag"), ver)
		}
	}

	page := get("/art/structure?w=3")
	if page.Code != 200 {
		t.Fatalf("/art/structure?w=3 = %d", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{
		"/static/structure.js?v=" + structureJSVer,
		"/static/vendor/three.module.min.js?v=" + threeJSVer,
		"/static/vendor/OrbitControls.js?v=" + orbitJSVer,
		`id="structure-plate"`,
		"/api/art/structure?w=3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("structure page missing %q", want)
		}
	}
	// The fallback SVG is still inline, with no baked-in title block.
	if !strings.Contains(body, "<svg") || strings.Contains(body, "STRUCTURE · W=") {
		t.Error("structure page must keep the SVG fallback, without an in-SVG title block")
	}
}
