package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSecureHeadersOnEveryResponse pins the fleet-wide baseline. The audit
// found none of these on any app, on services reachable from the public
// internet that serve author-supplied content.
func TestSecureHeaders(t *testing.T) {
	h := SecureHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// TestMaxBodyRejectsOversize bounds what a write endpoint will read.
func TestMaxBody(t *testing.T) {
	h := MaxBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}), 1024)

	big := httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 4096)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize POST = %d, want 413", rec.Code)
	}

	ok := httptest.NewRequest("POST", "/", strings.NewReader("small"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, ok)
	if rec.Code != http.StatusOK {
		t.Errorf("small POST = %d, want 200", rec.Code)
	}

	// A GET has no body worth capping and must not be wrapped.
	get := httptest.NewRequest("GET", "/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK {
		t.Errorf("GET = %d, want 200", rec.Code)
	}
}

// TestLogoutIsNotAGetSideEffect pins the CSRF fix. As a GET, any third-party
// page could end a fleet session with an <img> tag.
func TestLogoutIsNotAGetSideEffect(t *testing.T) {
	a := &Auth{Password: "pw"}

	rec := httptest.NewRecorder()
	a.HandleLogout(rec, httptest.NewRequest("GET", "/logout", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /logout = %d, want 200 (a confirmation page)", rec.Code)
	}
	if c := rec.Result().Cookies(); len(c) != 0 {
		t.Errorf("GET /logout cleared a cookie — still a state-changing GET: %v", c)
	}
	if !strings.Contains(rec.Body.String(), `method="post"`) {
		t.Error("GET /logout should render a POST form")
	}

	// A cross-origin POST is refused.
	req := httptest.NewRequest("POST", "/logout", nil)
	req.Host = "content.farfield.systems"
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	a.HandleLogout(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST /logout = %d, want 403", rec.Code)
	}

	// A same-origin POST logs out.
	req = httptest.NewRequest("POST", "/logout", nil)
	req.Host = "content.farfield.systems"
	req.Header.Set("Origin", "https://content.farfield.systems")
	rec = httptest.NewRecorder()
	a.HandleLogout(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("same-origin POST /logout = %d, want 303", rec.Code)
	}
}

// TestPlateShipsItsOwnFonts — the 404 page is served by every app, so a font
// CDN link here leaked a visitor IP on every 404 anywhere in the fleet.
func TestPlateShipsItsOwnFonts(t *testing.T) {
	for _, bad := range []string{"fonts.googleapis.com", "fonts.gstatic.com"} {
		if strings.Contains(plate404HTML, bad) {
			t.Errorf("404 plate still references %s", bad)
		}
	}
	if !strings.Contains(plate404HTML, "@font-face") {
		t.Error("404 plate carries no vendored faces")
	}
}

// TestMaxBodyExceptSkipsStreamingRoutes — the streaming uploads must stay
// uncapped, or a 100 MiB .ipa becomes a 413.
func TestMaxBodyExceptSkipsStreamingRoutes(t *testing.T) {
	h := MaxBodyExcept(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}), 1024, PathPrefixSkipper("/upload"))

	big := strings.Repeat("x", 4096)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/upload", strings.NewReader(big)))
	if rec.Code != http.StatusOK {
		t.Errorf("POST /upload = %d, want 200 — streaming route must not be capped", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/entries", strings.NewReader(big)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("POST /api/entries = %d, want 413 — ordinary write must be capped", rec.Code)
	}
}
