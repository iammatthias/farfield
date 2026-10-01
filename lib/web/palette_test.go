package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
)

func TestFleetOrigin(t *testing.T) {
	t.Setenv("FARFIELD_FLEET", "")
	for origin, want := range map[string]bool{
		"https://farfield.systems":          true,
		"https://content.farfield.systems":  true,
		"http://content.farfield.systems":   false, // never over plain http in production
		"https://farfield.systems.evil.com": false,
		"https://evilfarfield.systems":      false,
		"http://127.0.0.1:8787":             false, // local only under FARFIELD_FLEET=local
		"":                                  false,
		"null":                              false,
	} {
		if got := fleetOrigin(origin); got != want {
			t.Errorf("fleetOrigin(%q) = %v, want %v", origin, got, want)
		}
	}

	t.Setenv("FARFIELD_FLEET", "local")
	t.Setenv("FARFIELD_FLEET_HOST", "100.64.0.1")
	for origin, want := range map[string]bool{
		"http://100.64.0.1:8788": true,
		"http://127.0.0.1:8787":  true,
		"http://100.64.0.2:8788": false,
	} {
		if got := fleetOrigin(origin); got != want {
			t.Errorf("local fleetOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
}

func paletteServer(t *testing.T) *http.ServeMux {
	t.Helper()
	rd := &Renderer{App: "feed", Nav: []NavItem{{Label: "Posts", URL: "/posts"}, {Label: "Log out", URL: "/logout"}}}
	mux := http.NewServeMux()
	rd.MountPalette(mux, &Auth{}, func(r *http.Request) []PaletteItem {
		return []PaletteItem{{Kind: "record", Title: "a private post", URL: "/posts/1"}}
	})
	if !rd.palette {
		t.Fatal("MountPalette did not turn the menu script on")
	}
	return mux
}

func decodePalette(t *testing.T, w *httptest.ResponseRecorder) (out struct {
	App      string        `json:"app"`
	SignedIn bool          `json:"signedIn"`
	Items    []PaletteItem `json:"items"`
}) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("palette JSON: %v: %s", err, w.Body.String())
	}
	return out
}

func TestPaletteSignedOutListsOnlyTheWayIn(t *testing.T) {
	mux := paletteServer(t)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/palette", nil))
	out := decodePalette(t, w)
	if out.SignedIn || len(out.Items) != 1 || out.Items[0].URL != "/login" {
		t.Fatalf("signed out = %+v, want only the sign-in item", out)
	}
	if strings.Contains(w.Body.String(), "private post") {
		t.Fatal("records leaked to a signed-out request")
	}
}

func TestPaletteSignedIn(t *testing.T) {
	fleetOnceAuth.Do(func() {})
	fleetSecret, cookieDomain = "fleet-test-secret", ""
	defer func() { fleetSecret, cookieDomain = "", "" }()

	mux := paletteServer(t)
	r := httptest.NewRequest("GET", "/palette", nil)
	r.AddCookie(&http.Cookie{Name: "session", Value: auth.SignSession("fleet-test-secret", sessionEpoch(), time.Now().Add(time.Hour))})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	out := decodePalette(t, w)
	var titles []string
	for _, it := range out.Items {
		titles = append(titles, it.Title)
	}
	got := strings.Join(titles, ",")
	if got != "feed,Posts,a private post" {
		t.Fatalf("items = %s, want home, nav without log out, then the source", got)
	}
}

func TestPaletteCORS(t *testing.T) {
	t.Setenv("FARFIELD_FLEET", "")
	mux := paletteServer(t)

	r := httptest.NewRequest("GET", "/palette", nil)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if v := w.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Fatalf("a foreign origin got Allow-Origin %q", v)
	}

	r = httptest.NewRequest("OPTIONS", "/palette", nil)
	r.Header.Set("Origin", "https://content.farfield.systems")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "https://content.farfield.systems" ||
		w.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("preflight headers = %v", w.Header())
	}
}

func TestRequireFleetSession(t *testing.T) {
	t.Setenv("FARFIELD_FLEET", "")
	a := &Auth{}
	reached := false
	h := a.RequireFleetSession(func(w http.ResponseWriter, r *http.Request) { reached = true })

	// a preflight carries no cookie and must still be answered
	r := httptest.NewRequest("OPTIONS", "/palette/ask", nil)
	r.Header.Set("Origin", "https://feed.farfield.systems")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusNoContent || reached {
		t.Fatalf("preflight = %d (reached %v)", w.Code, reached)
	}

	// no session is a JSON 401, not a redirect a fetch would follow
	r = httptest.NewRequest("POST", "/palette/ask", nil)
	r.Header.Set("Origin", "https://feed.farfield.systems")
	w = httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusUnauthorized || reached {
		t.Fatalf("no session = %d (reached %v)", w.Code, reached)
	}
}

func TestPaletteFleetOmitsBackupInProduction(t *testing.T) {
	t.Setenv("FARFIELD_FLEET", "")
	for _, f := range paletteFleet() {
		if f["name"] == "backup" {
			t.Fatal("backup is tailnet-only; a browser on the public fleet cannot reach it")
		}
		if f["name"] == "apex" && f["url"] != "https://farfield.systems" {
			t.Fatalf("apex = %s", f["url"])
		}
	}
}

// TestCORSLeavesThePaletteAlone: an app's permissive API CORS answered the
// menu's preflight with "*" and no credentials, so every Ask from another app
// failed in the browser.
func TestCORSLeavesThePaletteAlone(t *testing.T) {
	t.Setenv("FARFIELD_FLEET", "")
	a := &Auth{}
	mux := http.NewServeMux()
	mux.HandleFunc("OPTIONS /palette/ask", a.RequireFleetSession(func(http.ResponseWriter, *http.Request) {}))
	h := CORS(mux, "GET", "POST", "OPTIONS")

	r := httptest.NewRequest("OPTIONS", "/palette/ask", nil)
	r.Header.Set("Origin", "https://qr.farfield.systems")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://qr.farfield.systems" {
		t.Fatalf("Allow-Origin = %q, want the fleet origin echoed", got)
	}
	if w.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatal("preflight without credentials")
	}
}
