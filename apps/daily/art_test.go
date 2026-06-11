package main

import (
	"bytes"
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

	// Out-of-archive dates do not exist.
	for _, path := range []string{"/art/2019-12-31", "/art/2019-12-31.svg",
		"/art/" + addDays(todayUTC(), 1), "/api/art/2019-12-31"} {
		if rec := get(path, ""); rec.Code != 404 {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}
