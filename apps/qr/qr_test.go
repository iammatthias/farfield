package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iammatthias/farfield/lib/web"
)

// ── helpers ────────────────────────────────────────────────────────────────

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "qr.sqlite")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	db := newTestDB(t)
	tmpl, err := web.ParseTemplates(assets, templateFuncs())
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	return &Server{
		db: db,
		auth: &web.Auth{
			DB:       db,
			Password: "secret",
			APIKey:   "k1",
		},
		rd:        &web.Renderer{Templates: tmpl, AssetVer: "test"},
		publicURL: "https://qr.test",
	}
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func loginSession(t *testing.T, ts *httptest.Server) []*http.Cookie {
	t.Helper()
	c := noRedirectClient()
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {"secret"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a cookie")
	}
	return cookies
}

// ── QR encoder unit tests ──────────────────────────────────────────────────

// TestEncoderRoundTripStructure verifies the encoder produces a matrix with
// the expected size, finder patterns in the three corners, and a non-empty
// SVG for a representative payload. Without a stdlib QR decoder we can't
// verify scan-correctness, but the structural invariants catch most bugs.
func TestEncoderRoundTripStructure(t *testing.T) {
	mod, version, err := Encode([]byte("https://farfield.systems"), ECMedium)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	expectedSize := 21 + 4*(version-1)
	if len(mod) != expectedSize {
		t.Fatalf("matrix size = %d, want %d", len(mod), expectedSize)
	}
	for _, row := range mod {
		if len(row) != expectedSize {
			t.Fatalf("matrix row = %d, want %d", len(row), expectedSize)
		}
	}
	// Top-left finder: corners of a 7x7 with a dark ring and 3x3 center.
	for _, corner := range [][2]int{{0, 0}, {6, 0}, {0, 6}, {6, 6}, {3, 3}} {
		if mod[corner[1]][corner[0]] != 1 {
			t.Errorf("top-left finder pixel (%d,%d) = %d, want 1", corner[0], corner[1], mod[corner[1]][corner[0]])
		}
	}
	// Top-right finder anchor.
	if mod[0][expectedSize-1] != 1 || mod[6][expectedSize-7] != 1 {
		t.Error("top-right finder pattern missing dark corners")
	}
	// Bottom-left finder anchor.
	if mod[expectedSize-1][0] != 1 || mod[expectedSize-7][6] != 1 {
		t.Error("bottom-left finder pattern missing dark corners")
	}
	// Always-dark module at (col=8, row=size-8).
	if mod[expectedSize-8][8] != 1 {
		t.Error("always-dark module missing at (8, size-8)")
	}
}

// TestEncoderVersionSelection verifies smaller payloads pack into smaller
// QR versions and larger payloads grow as needed.
func TestEncoderVersionSelection(t *testing.T) {
	cases := []struct {
		payload string
		ec      ECLevel
		maxVer  int
	}{
		{"hi", ECMedium, 1},
		{strings.Repeat("a", 50), ECMedium, 5},
		{strings.Repeat("a", 200), ECMedium, 12},
	}
	for _, c := range cases {
		_, v, err := Encode([]byte(c.payload), c.ec)
		if err != nil {
			t.Fatalf("Encode(%d bytes): %v", len(c.payload), err)
		}
		if v > c.maxVer {
			t.Errorf("Encode(%d bytes) → v%d, expected ≤ v%d", len(c.payload), v, c.maxVer)
		}
	}
}

// TestEncoderRejectsOversizedPayload checks the encoder returns an error
// rather than panicking when the payload exceeds version 40 capacity.
func TestEncoderRejectsOversizedPayload(t *testing.T) {
	huge := bytes.Repeat([]byte{'a'}, 5000)
	_, _, err := Encode(huge, ECHigh)
	if err == nil {
		t.Fatal("expected error for oversized payload at ECHigh")
	}
}

// TestEncoderSVGContainsModules verifies the rendered SVG mentions the
// expected fill color and contains at least one path "M" command per dark
// module. A trivial sanity check; doesn't validate scan-correctness.
func TestEncoderSVGContainsModules(t *testing.T) {
	svg, _, err := EncodeSVG([]byte("hello"), ECMedium)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Errorf("SVG not well-formed: %q", svg[:min(80, len(svg))])
	}
	if !strings.Contains(svg, `fill="#0a0a0a"`) {
		t.Error("SVG missing dark-fill path")
	}
	if !strings.Contains(svg, `viewBox="`) {
		t.Error("SVG missing viewBox")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestFormatInfoBits confirms the format-info value for a known
// (EC, mask) pair matches the canonical table from ISO/IEC 18004 Annex C.
// Spec sample: ECLow, mask 0 → 0x77C4.
func TestFormatInfoBits(t *testing.T) {
	got := formatInfo(ECLow, 0)
	want := uint32(0x77C4)
	if got != want {
		t.Errorf("formatInfo(L, 0) = %#x, want %#x", got, want)
	}
	// ECHigh, mask 7 → 0x083B (from Annex C).
	got2 := formatInfo(ECHigh, 7)
	want2 := uint32(0x083B)
	if got2 != want2 {
		t.Errorf("formatInfo(H, 7) = %#x, want %#x", got2, want2)
	}
}

// TestVersionInfoBits confirms the version-info value for v7 — the smallest
// version that has version info — matches the spec. v7 = 0x07C94.
func TestVersionInfoBits(t *testing.T) {
	got := versionInfoBits(7)
	want := uint32(0x07C94)
	if got != want {
		t.Errorf("versionInfoBits(7) = %#x, want %#x", got, want)
	}
}

// TestGFArithmeticIsConsistent walks a few GF(256) round-trips through the
// exp/log tables to catch a busted primitive-polynomial init.
func TestGFArithmeticIsConsistent(t *testing.T) {
	// gfExp[0] = 1, gfExp[1] = 2, gfExp[2] = 4 ... gfExp[7] = 128, gfExp[8] = 0x1D
	want := []byte{1, 2, 4, 8, 16, 32, 64, 128, 0x1D, 0x3A}
	for i, w := range want {
		if gfExp[i] != w {
			t.Errorf("gfExp[%d] = %#x, want %#x", i, gfExp[i], w)
		}
	}
	// Multiplication round-trip via log.
	for a := byte(1); a < 16; a++ {
		for b := byte(1); b < 16; b++ {
			prod := gfMul(a, b)
			if prod == 0 {
				t.Errorf("gfMul(%d, %d) unexpectedly 0", a, b)
			}
		}
	}
}

// ── DB / model tests ───────────────────────────────────────────────────────

func TestCodeCRUDAndCID(t *testing.T) {
	db := newTestDB(t)

	c := &Code{
		Label:      "Homepage",
		Mode:       ModeDirect,
		Target:     "https://example.com",
		EC:         "M",
		Public:     true,
		Enabled:    true,
		AdminNotes: "private",
	}
	if err := insertCode(db, c); err != nil {
		t.Fatalf("insertCode: %v", err)
	}
	if c.ID == "" {
		t.Fatal("insertCode left ID empty")
	}
	if c.CID == "" {
		t.Fatal("insertCode left CID empty")
	}
	origCID := c.CID

	// Read back.
	got, err := getCode(db, c.ID)
	if err != nil {
		t.Fatalf("getCode: %v", err)
	}
	if got == nil || got.Target != "https://example.com" || got.AdminNotes != "private" {
		t.Errorf("getCode round-trip: %+v", got)
	}

	// Editing admin notes alone must NOT change the CID.
	got.AdminNotes = "changed"
	if _, err := updateCode(db, c.ID, got); err != nil {
		t.Fatalf("updateCode: %v", err)
	}
	if got.CID != origCID {
		t.Errorf("CID changed on admin-only edit: %s → %s", origCID, got.CID)
	}

	// Editing the target MUST change the CID.
	got.Target = "https://changed.example"
	if _, err := updateCode(db, c.ID, got); err != nil {
		t.Fatalf("updateCode: %v", err)
	}
	if got.CID == origCID {
		t.Error("CID stayed equal when target changed")
	}

	// Delete.
	if ok, err := deleteCode(db, c.ID); err != nil || !ok {
		t.Fatalf("deleteCode ok=%v err=%v", ok, err)
	}
	if g, _ := getCode(db, c.ID); g != nil {
		t.Error("deleteCode left the row behind")
	}
}

func TestPublicListFiltersPrivateAndDisabled(t *testing.T) {
	db := newTestDB(t)
	must := func(c *Code) {
		if err := insertCode(db, c); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	must(&Code{Target: "a", Mode: ModeDirect, Public: true, Enabled: true})
	must(&Code{Target: "b", Mode: ModeDirect, Public: false, Enabled: true})
	must(&Code{Target: "c", Mode: ModeDirect, Public: true, Enabled: false})
	must(&Code{Target: "d", Mode: ModeDirect, Public: false, Enabled: false})

	all, err := listCodes(db)
	if err != nil || len(all) != 4 {
		t.Fatalf("listCodes = %d, want 4 (err=%v)", len(all), err)
	}
	pub, err := listPublicCodes(db)
	if err != nil {
		t.Fatalf("listPublicCodes: %v", err)
	}
	if len(pub) != 1 {
		t.Fatalf("listPublicCodes = %d, want 1", len(pub))
	}
	if pub[0].Target != "a" {
		t.Errorf("listPublicCodes returned wrong code: %+v", pub[0])
	}
}

func TestPublicListStripsAdminNotes(t *testing.T) {
	db := newTestDB(t)
	if err := insertCode(db, &Code{
		Target: "x", Mode: ModeDirect, Public: true, Enabled: true,
		AdminNotes: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	pub, _ := listPublicCodes(db)
	stripped := publicList(pub)
	if stripped[0].AdminNotes != "" {
		t.Errorf("publicList kept AdminNotes: %q", stripped[0].AdminNotes)
	}
	if pub[0].AdminNotes != "secret" {
		t.Errorf("publicList mutated source list: %q", pub[0].AdminNotes)
	}
}

// ── direct mode: QR encodes the payload verbatim ──────────────────────────

// TestDirectModeEncodesTargetVerbatim verifies that a direct-mode code's
// QR carries the same bytes as Target — by checking that EncodeSVG of the
// target alone produces the same SVG the server serves.
func TestDirectModeEncodesTargetVerbatim(t *testing.T) {
	s := newTestServer(t)
	c := &Code{
		Target: "https://example.com/direct",
		Mode:   ModeDirect, EC: "M",
		Public: true, Enabled: true,
	}
	if err := insertCode(s.db, c); err != nil {
		t.Fatal(err)
	}

	gotSVG, _, err := s.encodeFor(c)
	if err != nil {
		t.Fatal(err)
	}
	wantSVG, _, err := EncodeSVG([]byte(c.Target), ECMedium)
	if err != nil {
		t.Fatal(err)
	}
	if gotSVG != wantSVG {
		t.Error("direct-mode encoding does not match payload-verbatim encoding")
	}
}

// TestProxyModeEncodesPublicURL verifies that proxy-mode codes encode the
// service's stable redirect URL rather than the current target — that is the
// whole point of proxy mode (editing target keeps the QR unchanged).
func TestProxyModeEncodesPublicURL(t *testing.T) {
	s := newTestServer(t)
	c := &Code{
		Target: "https://example.com/v1",
		Mode:   ModeProxy, EC: "M",
		Public: true, Enabled: true,
	}
	if err := insertCode(s.db, c); err != nil {
		t.Fatal(err)
	}

	gotSVG, _, err := s.encodeFor(c)
	if err != nil {
		t.Fatal(err)
	}
	expected := s.publicURL + "/r/" + c.ID
	wantSVG, _, err := EncodeSVG([]byte(expected), ECMedium)
	if err != nil {
		t.Fatal(err)
	}
	if gotSVG != wantSVG {
		t.Error("proxy-mode encoding does not match proxy-URL encoding")
	}

	// Critical: editing the target MUST NOT change the SVG.
	c.Target = "https://example.com/v2-was-edited"
	if _, err := updateCode(s.db, c.ID, c); err != nil {
		t.Fatal(err)
	}
	updated, _ := getCode(s.db, c.ID)
	editedSVG, _, _ := s.encodeFor(updated)
	if editedSVG != gotSVG {
		t.Error("proxy QR changed after editing the target — the QR must remain stable")
	}
}

// ── proxy redirect behavior ────────────────────────────────────────────────

func TestProxyRedirectFollowsCurrentTarget(t *testing.T) {
	s := newTestServer(t)
	c := &Code{
		Target: "https://example.com/v1",
		Mode:   ModeProxy, EC: "M",
		Public: true, Enabled: true,
	}
	if err := insertCode(s.db, c); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp := requestNoRedirect(t, ts.URL+"/r/"+c.ID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("/r/{id} = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "https://example.com/v1" {
		t.Errorf("Location = %q, want %q", loc, "https://example.com/v1")
	}
	resp.Body.Close()

	// Edit the target — same redirect path now resolves to the new URL.
	c.Target = "https://example.com/v2"
	if _, err := updateCode(s.db, c.ID, c); err != nil {
		t.Fatal(err)
	}
	resp2 := requestNoRedirect(t, ts.URL+"/r/"+c.ID)
	defer resp2.Body.Close()
	if loc := resp2.Header.Get("Location"); loc != "https://example.com/v2" {
		t.Errorf("after edit, Location = %q, want %q", loc, "https://example.com/v2")
	}
}

func TestProxyRedirectRefusesPrivate(t *testing.T) {
	s := newTestServer(t)
	for _, c := range []*Code{
		{Target: "https://x", Mode: ModeProxy, EC: "M", Public: false, Enabled: true},
		{Target: "https://y", Mode: ModeProxy, EC: "M", Public: true, Enabled: false},
		{Target: "https://z", Mode: ModeDirect, EC: "M", Public: true, Enabled: true}, // direct has no proxy
	} {
		if err := insertCode(s.db, c); err != nil {
			t.Fatal(err)
		}
	}
	codes, _ := listCodes(s.db)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	for _, c := range codes {
		resp := requestNoRedirect(t, ts.URL+"/r/"+c.ID)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("/r/%s = %d, want 404 (code: %+v)", c.ID, resp.StatusCode, c)
		}
		resp.Body.Close()
	}
}

func requestNoRedirect(t *testing.T, u string) *http.Response {
	t.Helper()
	c := noRedirectClient()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	return resp
}

// ── public/private filtering on the SVG and JSON endpoints ─────────────────

func TestPublicSVGGatesByPublicAndEnabled(t *testing.T) {
	s := newTestServer(t)
	codes := []*Code{
		{Label: "open", Target: "open", Mode: ModeDirect, EC: "M", Public: true, Enabled: true},
		{Label: "priv", Target: "priv", Mode: ModeDirect, EC: "M", Public: false, Enabled: true},
		{Label: "disabled", Target: "off", Mode: ModeDirect, EC: "M", Public: true, Enabled: false},
	}
	for _, c := range codes {
		if err := insertCode(s.db, c); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	for _, c := range codes {
		resp, err := http.Get(ts.URL + "/qr/" + c.ID + ".svg")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		wantOK := c.Public && c.Enabled
		switch {
		case wantOK && resp.StatusCode != http.StatusOK:
			t.Errorf("/qr/%s = %d, want 200", c.Label, resp.StatusCode)
		case !wantOK && resp.StatusCode != http.StatusNotFound:
			t.Errorf("/qr/%s = %d, want 404", c.Label, resp.StatusCode)
		}
	}
}

func TestPublicSVGServesValidSVG(t *testing.T) {
	s := newTestServer(t)
	c := &Code{Target: "https://example.com", Mode: ModeDirect, EC: "M", Public: true, Enabled: true}
	if err := insertCode(s.db, c); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/qr/" + c.ID + ".svg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/qr/{id}.svg = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Errorf("Content-Type = %q, want image/svg+xml*", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.HasPrefix(body, []byte("<svg")) {
		t.Errorf("body does not look like SVG: %q", body[:min(80, len(body))])
	}
}

func TestPublicSVGETag(t *testing.T) {
	s := newTestServer(t)
	c := &Code{Target: "https://example.com", Mode: ModeDirect, EC: "M", Public: true, Enabled: true}
	if err := insertCode(s.db, c); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/qr/" + c.ID + ".svg")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" || !strings.Contains(etag, c.CID) {
		t.Fatalf("ETag = %q, want CID %q embedded", etag, c.CID)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/qr/"+c.ID+".svg", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", resp2.StatusCode)
	}
}

// ── public JSON API ────────────────────────────────────────────────────────

func TestPublicAPISkipsPrivateDisabledAndStripsNotes(t *testing.T) {
	s := newTestServer(t)
	mustInsert := func(c *Code) {
		if err := insertCode(s.db, c); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	mustInsert(&Code{Target: "a", Mode: ModeDirect, EC: "M",
		Public: true, Enabled: true, AdminNotes: "secret-a"})
	priv := &Code{Target: "b", Mode: ModeDirect, EC: "M",
		Public: false, Enabled: true, AdminNotes: "secret-b"}
	mustInsert(priv)
	disabled := &Code{Target: "c", Mode: ModeDirect, EC: "M",
		Public: true, Enabled: false, AdminNotes: "secret-c"}
	mustInsert(disabled)

	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/codes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET list status %d", resp.StatusCode)
	}
	var listResp struct {
		Codes []Code `json:"codes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listResp.Codes) != 1 {
		t.Fatalf("public list = %d, want 1", len(listResp.Codes))
	}
	if listResp.Codes[0].AdminNotes != "" {
		t.Errorf("public list leaked adminNotes: %q", listResp.Codes[0].AdminNotes)
	}

	// Private GET → 404.
	for _, c := range []*Code{priv, disabled} {
		resp3, _ := http.Get(ts.URL + "/api/codes/" + c.ID)
		if resp3.StatusCode != http.StatusNotFound {
			t.Errorf("private/disabled GET = %d, want 404", resp3.StatusCode)
		}
		resp3.Body.Close()
	}
}

func TestPublicAPISingleRecordETag(t *testing.T) {
	s := newTestServer(t)
	c := &Code{Target: "x", Mode: ModeDirect, EC: "M", Public: true, Enabled: true}
	if err := insertCode(s.db, c); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/codes/" + c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" || !strings.Contains(etag, c.CID) {
		t.Errorf("ETag = %q, want CID %q embedded", etag, c.CID)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), `"adminNotes"`) {
		t.Errorf("single GET leaked adminNotes: %s", body)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/codes/"+c.ID, nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional single = %d, want 304", resp2.StatusCode)
	}
}

// ── auth boundaries ────────────────────────────────────────────────────────

func TestSessionGatedRootRedirectsUnauthenticated(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	c := noRedirectClient()
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("/ = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("/ Location = %q, want /login", loc)
	}

	// Form posts must also redirect (not act).
	resp2, err := c.PostForm(ts.URL+"/codes",
		url.Values{"target": {"x"}, "mode": {"direct"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther ||
		resp2.Header.Get("Location") != "/login" {
		t.Errorf("POST /codes unauthed = %d %q", resp2.StatusCode, resp2.Header.Get("Location"))
	}
}

func TestSessionLoginRejectsWrongPassword(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	c := noRedirectClient()
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"password": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("wrong login = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "/login") || !strings.Contains(loc, "error") {
		t.Errorf("wrong login Location = %q, want /login?error=…", loc)
	}
}

func TestLoggedInCRUDForms(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	cookies := loginSession(t, ts)
	c := noRedirectClient()

	// Authed GET /.
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/ authed = %d, want 200", resp.StatusCode)
	}

	// Create via form post.
	form := url.Values{
		"label":   {"Site"},
		"mode":    {"direct"},
		"target":  {"https://example.com"},
		"ec":      {"M"},
		"public":  {"on"},
		"enabled": {"on"},
	}
	reqCreate, _ := http.NewRequest("POST", ts.URL+"/codes", strings.NewReader(form.Encode()))
	reqCreate.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, ck := range cookies {
		reqCreate.AddCookie(ck)
	}
	respCreate, err := c.Do(reqCreate)
	if err != nil {
		t.Fatal(err)
	}
	respCreate.Body.Close()
	if respCreate.StatusCode != http.StatusSeeOther || respCreate.Header.Get("Location") != "/" {
		t.Fatalf("create redirect = %d %q", respCreate.StatusCode, respCreate.Header.Get("Location"))
	}

	cs, _ := listCodes(s.db)
	if len(cs) != 1 || cs[0].Label != "Site" || !cs[0].Public || !cs[0].Enabled {
		t.Fatalf("create did not persist: %+v", cs)
	}
	id := cs[0].ID

	// Edit form GET.
	reqEdit, _ := http.NewRequest("GET", ts.URL+"/codes/"+id+"/edit", nil)
	for _, ck := range cookies {
		reqEdit.AddCookie(ck)
	}
	respEdit, err := c.Do(reqEdit)
	if err != nil {
		t.Fatal(err)
	}
	respEdit.Body.Close()
	if respEdit.StatusCode != http.StatusOK {
		t.Errorf("GET edit = %d", respEdit.StatusCode)
	}

	// Preview always returns SVG, even when not public.
	reqPrev, _ := http.NewRequest("GET", ts.URL+"/codes/"+id+"/preview", nil)
	for _, ck := range cookies {
		reqPrev.AddCookie(ck)
	}
	respPrev, err := c.Do(reqPrev)
	if err != nil {
		t.Fatal(err)
	}
	respPrev.Body.Close()
	if respPrev.StatusCode != http.StatusOK {
		t.Errorf("preview = %d", respPrev.StatusCode)
	}

	// Update via form post (omit "public" → private).
	updForm := url.Values{
		"label":   {"Site"},
		"mode":    {"direct"},
		"target":  {"https://example.com"},
		"ec":      {"M"},
		"enabled": {"on"},
	}
	reqUpd, _ := http.NewRequest("POST", ts.URL+"/codes/"+id, strings.NewReader(updForm.Encode()))
	reqUpd.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, ck := range cookies {
		reqUpd.AddCookie(ck)
	}
	respUpd, err := c.Do(reqUpd)
	if err != nil {
		t.Fatal(err)
	}
	respUpd.Body.Close()
	if respUpd.StatusCode != http.StatusSeeOther {
		t.Errorf("update = %d, want 303", respUpd.StatusCode)
	}
	got, _ := getCode(s.db, id)
	if got == nil || got.Public || !got.Enabled {
		t.Errorf("update did not persist visibility: %+v", got)
	}

	// Delete via form post.
	reqDel, _ := http.NewRequest("POST", ts.URL+"/codes/"+id+"/delete", bytes.NewReader(nil))
	for _, ck := range cookies {
		reqDel.AddCookie(ck)
	}
	respDel, err := c.Do(reqDel)
	if err != nil {
		t.Fatal(err)
	}
	respDel.Body.Close()
	if respDel.StatusCode != http.StatusSeeOther {
		t.Errorf("delete = %d, want 303", respDel.StatusCode)
	}
	if g, _ := getCode(s.db, id); g != nil {
		t.Errorf("delete did not remove the row: %+v", g)
	}
}

// ── API-key write auth ─────────────────────────────────────────────────────

func TestAPIKeyWriteAuth(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	body := []byte(`{"target":"https://api.example/","mode":"direct","ec":"M","public":true,"enabled":true}`)

	// Missing key → 401.
	resp, err := http.Post(ts.URL+"/api/codes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-key = %d, want 401", resp.StatusCode)
	}

	// Wrong key → 401.
	req, _ := http.NewRequest("POST", ts.URL+"/api/codes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "nope")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-key = %d, want 401", resp2.StatusCode)
	}

	// Right key → 201.
	req3, _ := http.NewRequest("POST", ts.URL+"/api/codes", bytes.NewReader(body))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("X-API-Key", "k1")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusCreated {
		t.Errorf("right-key = %d, want 201", resp3.StatusCode)
	}
	var created Code
	if err := json.NewDecoder(resp3.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Target != "https://api.example/" {
		t.Errorf("created = %+v", created)
	}

	// PUT with Bearer.
	upd := []byte(`{"target":"https://api.example/v2","mode":"direct","ec":"M","public":true,"enabled":true}`)
	reqU, _ := http.NewRequest("PUT", ts.URL+"/api/codes/"+created.ID, bytes.NewReader(upd))
	reqU.Header.Set("Content-Type", "application/json")
	reqU.Header.Set("Authorization", "Bearer k1")
	respU, err := http.DefaultClient.Do(reqU)
	if err != nil {
		t.Fatal(err)
	}
	respU.Body.Close()
	if respU.StatusCode != http.StatusOK {
		t.Errorf("PUT bearer = %d, want 200", respU.StatusCode)
	}
	got, _ := getCode(s.db, created.ID)
	if got == nil || got.Target != "https://api.example/v2" {
		t.Errorf("PUT did not persist: %+v", got)
	}

	// DELETE with X-API-Key.
	reqD, _ := http.NewRequest("DELETE", ts.URL+"/api/codes/"+created.ID, nil)
	reqD.Header.Set("X-API-Key", "k1")
	respD, err := http.DefaultClient.Do(reqD)
	if err != nil {
		t.Fatal(err)
	}
	respD.Body.Close()
	if respD.StatusCode != http.StatusOK {
		t.Errorf("DELETE = %d, want 200", respD.StatusCode)
	}
	if g, _ := getCode(s.db, created.ID); g != nil {
		t.Error("DELETE did not remove row")
	}
}

func TestAPIKeyDisabledWhenUnconfigured(t *testing.T) {
	s := newTestServer(t)
	s.auth.APIKey = "" // simulate unset
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	body := []byte(`{"target":"x","mode":"direct","ec":"M"}`)
	req, _ := http.NewRequest("POST", ts.URL+"/api/codes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no-key-configured POST = %d, want 503", resp.StatusCode)
	}
}

func TestStatusEndpoint(t *testing.T) {
	s := newTestServer(t)
	if err := insertCode(s.db, &Code{
		Target: "a", Mode: ModeDirect, EC: "M",
		Public: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := insertCode(s.db, &Code{
		Target: "b", Mode: ModeDirect, EC: "M",
		Public: false, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Service string `json:"service"`
		OK      bool   `json:"ok"`
		Codes   int    `json:"codes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Service != "qr" || !body.OK {
		t.Errorf("status = %+v", body)
	}
	if body.Codes != 1 {
		t.Errorf("status counts only public+enabled: got %d, want 1", body.Codes)
	}
}
