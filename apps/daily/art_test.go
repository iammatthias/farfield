package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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

// TestZoneDeterminismAndDomainSeparation: the day's zone — its ink palette —
// must be a pure function of the date, drawn from its own hash domain so it
// is independent of the art seed (terrain) and the biome stream, with every
// index in range and the whole palette table exercised over a year.
func TestZoneDeterminismAndDomainSeparation(t *testing.T) {
	seen := map[int]bool{}
	for i := 0; i < 365; i++ {
		d := addDays(artifactEpoch, i)
		zi := zoneIndexFor(d)
		if zi != zoneIndexFor(d) {
			t.Fatalf("zone for %s is not deterministic", d)
		}
		if zi < 0 || zi >= len(zones) {
			t.Fatalf("zone for %s = %d, out of [0,%d)", d, zi, len(zones))
		}
		seen[zi] = true
	}
	if len(seen) != len(zones) {
		t.Errorf("a year exercises %d of %d zones — distribution looks broken", len(seen), len(zones))
	}
	// Domain separation: the zone stream must come from "art-zone:", not
	// from the art seed's domain — the prefix has to matter.
	differs := false
	for i := 0; i < 64 && !differs; i++ {
		d := addDays(artifactEpoch, i)
		h := sha256.Sum256([]byte(domainArt + ":" + d))
		differs = zoneIndexFor(d) != int(h[0])%len(zones)
	}
	if !differs {
		t.Error("zone indices mirror the art seed stream — domain separation is broken")
	}
	// The plate must carry the date's zone — same date, same zone, everywhere.
	n, _ := artDayIndex("2024-03-15")
	p := artPlotFor("2024-03-15", n)
	if p.ZoneIdx != zoneIndexFor("2024-03-15") || p.Zone.Name != zones[p.ZoneIdx].Name {
		t.Errorf("plot zone = %d %q, want %d", p.ZoneIdx, p.Zone.Name, zoneIndexFor("2024-03-15"))
	}
	// And every zone's ink set is complete.
	for i, z := range zones {
		if z.Name == "" || z.Wash == "" {
			t.Errorf("zone %d is missing a name or wash", i)
		}
		for _, ink := range z.Inks {
			if len(ink) != 7 || ink[0] != '#' {
				t.Errorf("zone %q has a malformed ink %q", z.Name, ink)
			}
		}
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

	// The structure page renders with its SVG fallback; stray ?w= queries
	// from old links are ignored, not 404s.
	if rec := get("/art/structure", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "<svg") {
		t.Errorf("/art/structure = %d", rec.Code)
	}
	if rec := get("/art/structure?w=16", ""); rec.Code != 200 {
		t.Errorf("/art/structure?w=16 = %d, want 200 (query ignored)", rec.Code)
	}

	// JSON carries coord, cid, and the day's zone.
	if rec := get("/api/art", ""); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `"coord"`) || !strings.Contains(rec.Body.String(), `"cid"`) ||
		!strings.Contains(rec.Body.String(), `"zone"`) {
		t.Errorf("/api/art = %d body %s", rec.Code, rec.Body.String())
	}

	// The page caption carries the zone name alongside the biome.
	if n, ok := artDayIndex("2024-03-15"); ok {
		p := artPlotFor("2024-03-15", n)
		page := get("/art/2024-03-15", "")
		want := "2024-03-15 · day " + strconv.FormatUint(p.N, 10) + " · " + p.Biome.Name + " · " + p.Zone.Name
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("art page caption missing %q", want)
		}
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
		W       int    `json:"w"`
		Side    int    `json:"side"`
		Bands   int    `json:"bands"`
		HFBands int    `json:"hfBands"`
		Date    string `json:"date"`
		Today   struct {
			Index uint64 `json:"index"`
			Coord []int  `json:"coord"`
		} `json:"today"`
		Cells []struct {
			I     uint64 `json:"i"`
			X     int    `json:"x"`
			Y     int    `json:"y"`
			Z     int    `json:"z"`
			Biome int    `json:"biome"`
			Zone  int    `json:"zone"`
			Age   int    `json:"age"`
			Today bool   `json:"today"`
			HF    []int  `json:"hf"`
		} `json:"cells"`
		Biomes []struct {
			Name   string   `json:"name"`
			Colors []string `json:"colors"`
		} `json:"biomes"`
		Zones []struct {
			Name   string   `json:"name"`
			Colors []string `json:"colors"`
			Wash   string   `json:"wash"`
		} `json:"zones"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.W != 6 || body.Side != artSide || body.Bands != structAgeBands {
		t.Errorf("w/side/bands = %d/%d/%d", body.W, body.Side, body.Bands)
	}
	if body.HFBands != len(biomes[0].Ramp) {
		t.Errorf("hfBands = %d, want %d", body.HFBands, len(biomes[0].Ramp))
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
	if len(body.Zones) != len(zones) {
		t.Fatalf("zones = %d, want %d", len(body.Zones), len(zones))
	}
	for i, z := range body.Zones {
		if z.Name != zones[i].Name || len(z.Colors) != 4 || z.Wash == "" {
			t.Errorf("zone %d = %+v", i, z)
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
		if c.Zone != zoneIndexFor(addDays(artifactEpoch, int(c.I))) {
			t.Fatalf("cell %d zone = %d, want its date's zone", c.I, c.Zone)
		}
		if int64(c.I) <= prev {
			t.Fatal("cells must arrive in ascending day order")
		}
		prev = int64(c.I)
		if c.Today && c.I != body.Today.Index {
			t.Errorf("today cell index %d != %d", c.I, body.Today.Index)
		}
		// Every cell carries its miniature tile: today's at the denser
		// grid, the rest at the standard one, levels within the ramp.
		wantHF := structTileGrid * structTileGrid
		if c.Today {
			wantHF = structTodayTileGrid * structTodayTileGrid
		}
		if len(c.HF) != wantHF {
			t.Fatalf("cell %d hf len = %d, want %d", c.I, len(c.HF), wantHF)
		}
		for _, lv := range c.HF {
			if lv < 0 || lv >= body.HFBands {
				t.Fatalf("cell %d tile level %d out of [0,%d)", c.I, lv, body.HFBands)
			}
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
}

// TestArtStructureAPIFull: without ?w the structure JSON carries the whole
// structure — every non-empty w-slice, each slice's cells in day order,
// with fully buried cells' tiles trimmed empty and every exposed cell
// keeping its tile.
func TestArtStructureAPIFull(t *testing.T) {
	s := testServer(t)
	h := s.routes()

	req := httptest.NewRequest("GET", "/api/art/structure", nil)
	req.Header.Set("Accept-Encoding", "identity")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/api/art/structure = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "public") {
		t.Errorf("cache-control = %q, want public", cc)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("structure JSON must carry an ETag")
	}
	var body struct {
		Side    int    `json:"side"`
		Bands   int    `json:"bands"`
		HFBands int    `json:"hfBands"`
		Date    string `json:"date"`
		Epoch   string `json:"epoch"`
		Today   struct {
			Index uint64 `json:"index"`
			Coord []int  `json:"coord"`
		} `json:"today"`
		Slices []struct {
			W     int `json:"w"`
			Cells []struct {
				I     uint64 `json:"i"`
				X     int    `json:"x"`
				Y     int    `json:"y"`
				Z     int    `json:"z"`
				Biome int    `json:"biome"`
				Zone  int    `json:"zone"`
				Age   int    `json:"age"`
				Today bool   `json:"today"`
				HF    []int  `json:"hf"`
			} `json:"cells"`
		} `json:"slices"`
		Biomes []struct {
			Name   string   `json:"name"`
			Colors []string `json:"colors"`
		} `json:"biomes"`
		Zones []struct {
			Name   string   `json:"name"`
			Colors []string `json:"colors"`
			Wash   string   `json:"wash"`
		} `json:"zones"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Side != artSide || body.Bands != structAgeBands || body.HFBands != len(biomes[0].Ramp) {
		t.Errorf("side/bands/hfBands = %d/%d/%d", body.Side, body.Bands, body.HFBands)
	}
	if len(body.Today.Coord) != 4 || body.Today.Index == 0 {
		t.Errorf("today = %+v, want index>0 and a 4-coord", body.Today)
	}
	if len(body.Biomes) != len(biomes) {
		t.Fatalf("biomes = %d, want %d", len(body.Biomes), len(biomes))
	}
	if len(body.Zones) != len(zones) {
		t.Fatalf("zones = %d, want %d", len(body.Zones), len(zones))
	}
	if len(body.Slices) == 0 {
		t.Fatal("full structure must carry at least one slice")
	}
	total := 0
	buriedTrimmed := 0
	todaySeen := 0
	prevW := -1
	for _, sl := range body.Slices {
		if sl.W <= prevW || sl.W >= artSide {
			t.Fatalf("slice w=%d out of order or off the lattice (prev %d)", sl.W, prevW)
		}
		prevW = sl.W
		if len(sl.Cells) == 0 {
			t.Fatalf("slice w=%d is empty — empty slices must be omitted", sl.W)
		}
		prev := int64(-1)
		for _, c := range sl.Cells {
			total++
			if c.X < 0 || c.X >= artSide || c.Y < 0 || c.Y >= artSide || c.Z < 0 || c.Z >= artSide {
				t.Fatalf("cell %d off the lattice: %+v", c.I, c)
			}
			if c.Biome < 0 || c.Biome >= len(biomes) || c.Age < 0 || c.Age >= structAgeBands {
				t.Fatalf("cell %d bad biome/age: %+v", c.I, c)
			}
			if c.Zone < 0 || c.Zone >= len(zones) {
				t.Fatalf("cell %d zone %d out of range", c.I, c.Zone)
			}
			if int64(c.I) <= prev {
				t.Fatalf("slice w=%d cells must arrive in ascending day order", sl.W)
			}
			prev = int64(c.I)
			if c.Today {
				todaySeen++
				if c.I != body.Today.Index || sl.W != body.Today.Coord[3] {
					t.Errorf("today cell %d in slice %d, want %d in slice %d",
						c.I, sl.W, body.Today.Index, body.Today.Coord[3])
				}
				if len(c.HF) != structTodayTileGrid*structTodayTileGrid {
					t.Errorf("today hf len = %d, want %d", len(c.HF), structTodayTileGrid*structTodayTileGrid)
				}
				continue
			}
			// A past cell either keeps its tile (exposed) or ships an empty
			// one (fully buried — it renders nothing).
			switch len(c.HF) {
			case 0:
				buriedTrimmed++
			case structTileGrid * structTileGrid:
				for _, lv := range c.HF {
					if lv < 0 || lv >= body.HFBands {
						t.Fatalf("cell %d tile level %d out of [0,%d)", c.I, lv, body.HFBands)
					}
				}
			default:
				t.Fatalf("cell %d hf len = %d, want 0 or %d", c.I, len(c.HF), structTileGrid*structTileGrid)
			}
		}
	}
	if want := int(body.Today.Index) + 1; total != want {
		t.Errorf("total cells = %d, want every day since the epoch (%d)", total, want)
	}
	if todaySeen != 1 {
		t.Errorf("today cells = %d, want exactly 1", todaySeen)
	}
	// Years in, the mass has an interior — the trim must be doing real work.
	if buriedTrimmed == 0 {
		t.Error("no buried cell was trimmed — the payload trim is not working")
	}
}

// TestArtPathAPI: the worldline JSON carries every day since the epoch in
// curve order — each with its 4-D coordinate, biome, and terrain tile —
// with consecutive days lattice neighbors (the Hilbert walk the ribbon
// renders), a content-CID ETag, public caching, and a raw size within the
// viewer's budget.
func TestArtPathAPI(t *testing.T) {
	s := testServer(t)
	h := s.routes()

	get := func(etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/art/path", nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		req.Header.Set("Accept-Encoding", "identity")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get("")
	if rec.Code != 200 {
		t.Fatalf("/api/art/path = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "public") {
		t.Errorf("cache-control = %q, want public", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("path JSON must carry an ETag")
	}
	if size := rec.Body.Len(); size > 700*1024 {
		t.Errorf("raw payload = %d bytes, want ≤ 700KB", size)
	}

	var body struct {
		Date    string `json:"date"`
		Epoch   string `json:"epoch"`
		N       uint64 `json:"n"`
		Side    int    `json:"side"`
		Bands   int    `json:"bands"`
		HFBands int    `json:"hfBands"`
		Days    []struct {
			I     uint64 `json:"i"`
			Coord []int  `json:"coord"`
			Biome int    `json:"biome"`
			Zone  int    `json:"zone"`
			HF    []int  `json:"hf"`
		} `json:"days"`
		Biomes []struct {
			Name   string   `json:"name"`
			Colors []string `json:"colors"`
		} `json:"biomes"`
		Zones []struct {
			Name   string   `json:"name"`
			Colors []string `json:"colors"`
			Wash   string   `json:"wash"`
		} `json:"zones"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Date != todayUTC() || body.Epoch != artifactEpoch {
		t.Errorf("date/epoch = %q/%q", body.Date, body.Epoch)
	}
	if body.Side != artSide || body.Bands != structAgeBands || body.HFBands != len(biomes[0].Ramp) {
		t.Errorf("side/bands/hfBands = %d/%d/%d", body.Side, body.Bands, body.HFBands)
	}
	if len(body.Biomes) != len(biomes) {
		t.Fatalf("biomes = %d, want %d", len(body.Biomes), len(biomes))
	}
	for i, b := range body.Biomes {
		if b.Name != biomes[i].Name || len(b.Colors) != 3 {
			t.Errorf("biome %d = %+v", i, b)
		}
	}
	if len(body.Zones) != len(zones) {
		t.Fatalf("zones = %d, want %d", len(body.Zones), len(zones))
	}
	for i, z := range body.Zones {
		if z.Name != zones[i].Name || len(z.Colors) != 4 || z.Wash != zones[i].Wash {
			t.Errorf("zone %d = %+v", i, z)
		}
	}
	// Every day from the epoch through today, in order, no gaps, no trims.
	if want := int(body.N) + 1; len(body.Days) != want {
		t.Fatalf("days = %d, want %d (every day since the epoch)", len(body.Days), want)
	}
	var prev []int
	for k, d := range body.Days {
		if d.I != uint64(k) {
			t.Fatalf("day %d carries index %d — days must arrive in curve order", k, d.I)
		}
		if len(d.Coord) != 4 {
			t.Fatalf("day %d coord = %v, want 4 axes", k, d.Coord)
		}
		for _, c := range d.Coord {
			if c < 0 || c >= artSide {
				t.Fatalf("day %d coord %v off the lattice", k, d.Coord)
			}
		}
		if d.Biome < 0 || d.Biome >= len(biomes) {
			t.Fatalf("day %d biome %d out of range", k, d.Biome)
		}
		if d.Zone != zoneIndexFor(addDays(artifactEpoch, k)) {
			t.Fatalf("day %d zone = %d, want its date's zone", k, d.Zone)
		}
		if len(d.HF) != structTileGrid*structTileGrid {
			t.Fatalf("day %d hf len = %d, want %d — every day keeps its terrain", k, len(d.HF), structTileGrid*structTileGrid)
		}
		for _, lv := range d.HF {
			if lv < 0 || lv >= body.HFBands {
				t.Fatalf("day %d tile level %d out of [0,%d)", k, lv, body.HFBands)
			}
		}
		// Consecutive days are 4-D lattice neighbors: exactly one axis
		// changes, by exactly 1 — the continuity the ribbon depends on.
		if prev != nil {
			diff := 0
			for a := 0; a < 4; a++ {
				step := d.Coord[a] - prev[a]
				if step < 0 {
					step = -step
				}
				diff += step
			}
			if diff != 1 {
				t.Fatalf("days %d→%d are not lattice neighbors: %v → %v", k-1, k, prev, d.Coord)
			}
		}
		prev = d.Coord
	}

	// The ETag revalidates.
	if rec := get(etag); rec.Code != 304 {
		t.Errorf("revalidation = %d, want 304", rec.Code)
	}
}

// TestArtTerrainAPI: the terrain JSON carries the day's full quantized
// heightfield — server-canonical, so the plate viewer never re-derives
// noise — plus the biome inks, with the plate CID as ETag, cached like
// /api/art, and 404s off the calendar.
func TestArtTerrainAPI(t *testing.T) {
	s := testServer(t)
	h := s.routes()

	get := func(path, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		req.Header.Set("Accept-Encoding", "identity")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for _, path := range []string{"/api/art/terrain", "/api/art/terrain/2024-03-15"} {
		rec := get(path, "")
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "public") {
			t.Errorf("%s cache-control = %q, want public", path, cc)
		}
		var body struct {
			Date  string `json:"date"`
			N     uint64 `json:"n"`
			Side  int    `json:"side"`
			Bands int    `json:"bands"`
			Biome struct {
				Index  int      `json:"index"`
				Name   string   `json:"name"`
				Colors []string `json:"colors"`
			} `json:"biome"`
			Zone struct {
				Index  int      `json:"index"`
				Name   string   `json:"name"`
				Colors []string `json:"colors"`
				Wash   string   `json:"wash"`
			} `json:"zone"`
			Levels []int  `json:"levels"`
			CID    string `json:"cid"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s unmarshal: %v", path, err)
		}
		if body.Side != plotSize || len(body.Levels) != plotSize*plotSize {
			t.Errorf("%s side/levels = %d/%d, want %d/%d", path, body.Side, len(body.Levels), plotSize, plotSize*plotSize)
		}
		if body.Bands != len(biomes[body.Biome.Index].Ramp) || len(body.Biome.Colors) != 3 {
			t.Errorf("%s bands/colors = %d/%d", path, body.Bands, len(body.Biome.Colors))
		}
		if body.Zone.Index != zoneIndexFor(body.Date) || body.Zone.Name != zones[body.Zone.Index].Name ||
			len(body.Zone.Colors) != 4 || body.Zone.Wash != zones[body.Zone.Index].Wash {
			t.Errorf("%s zone = %+v, want the date's zone", path, body.Zone)
		}
		for i, lv := range body.Levels {
			if lv < 0 || lv >= body.Bands {
				t.Fatalf("%s level[%d] = %d out of [0,%d)", path, i, lv, body.Bands)
			}
		}
		if body.CID == "" {
			t.Errorf("%s missing cid", path)
		}
		// The levels are the plate's own: the CID matches the SVG's, and the
		// ETag revalidates.
		n, _ := artDayIndex(body.Date)
		if p := artPlotFor(body.Date, n); p.CID != body.CID {
			t.Errorf("%s cid %q != plate cid %q", path, body.CID, p.CID)
		}
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s missing etag", path)
		}
		if rec := get(path, etag); rec.Code != 304 {
			t.Errorf("%s revalidation = %d, want 304", path, rec.Code)
		}
	}

	// A past day caches for a day; off-calendar dates do not exist.
	if cc := get("/api/art/terrain/2024-03-15", "").Header().Get("Cache-Control"); !strings.Contains(cc, "86400") {
		t.Errorf("past terrain cache-control = %q, want max-age=86400", cc)
	}
	for _, path := range []string{"/api/art/terrain/2019-12-31",
		"/api/art/terrain/" + addDays(todayUTC(), 1), "/api/art/terrain/nope"} {
		if rec := get(path, ""); rec.Code != 404 {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
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
		"/static/art.js":                     artJSVer,
		"/static/terrain.js":                 terrainJSVer,
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

	page := get("/art/structure")
	if page.Code != 200 {
		t.Fatalf("/art/structure = %d", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{
		"/static/structure.js?v=" + structureJSVer,
		"/static/terrain.js?v=" + terrainJSVer,
		"/static/vendor/three.module.min.js?v=" + threeJSVer,
		"/static/vendor/OrbitControls.js?v=" + orbitJSVer,
		`id="structure-plate"`,
		`data-api="/api/art/path"`,
		"Grown one cell per day since " + artifactEpoch,
		"today at the tip of the thread",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("structure page missing %q", want)
		}
	}
	// The fallback SVG is still inline, with no baked-in title block.
	if !strings.Contains(body, "<svg") || strings.Contains(body, "STRUCTURE · W=") {
		t.Error("structure page must keep the SVG fallback, without an in-SVG title block")
	}
	// The output is the focus: no w-axis apparatus anywhere on the page.
	for _, jargon := range []string{"W-SLICE", "W-AXIS", "fillmap", "CELLS ACCRETED", "CELLS PER SLICE"} {
		if strings.Contains(body, jargon) {
			t.Errorf("structure page still carries apparatus copy %q", jargon)
		}
	}

	// The plate page wires the same shared modules around its live tile,
	// keeping the inline SVG as the fallback inside the canvas mount.
	artPage := get("/art/2024-03-15")
	if artPage.Code != 200 {
		t.Fatalf("/art/2024-03-15 = %d", artPage.Code)
	}
	body = artPage.Body.String()
	for _, want := range []string{
		"/static/art.js?v=" + artJSVer,
		"/static/terrain.js?v=" + terrainJSVer,
		"/static/vendor/three.module.min.js?v=" + threeJSVer,
		`id="plate-canvas"`,
		"/api/art/terrain/2024-03-15",
		"<svg",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("art page missing %q", want)
		}
	}
	// One quiet caption line — the coordinate, order, and CID live in
	// /api/art, not on the page.
	for _, jargon := range []string{"hilbert order", "art-meta", "HYPERSTRUCTURE"} {
		if strings.Contains(body, jargon) {
			t.Errorf("art page still carries apparatus copy %q", jargon)
		}
	}
	if n, ok := artDayIndex("2024-03-15"); ok {
		if p := artPlotFor("2024-03-15", n); strings.Contains(body, p.CID) {
			t.Error("art page must not print the plate CID — it lives in /api/art")
		}
	}
}
