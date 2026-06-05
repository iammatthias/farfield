package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoutes(t *testing.T) {
	h, err := routes()
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	// The landing page is still the static SVG.
	if rec := get("/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<svg") {
		t.Errorf("GET / = %d, has svg = %v", rec.Code, strings.Contains(rec.Body.String(), "<svg"))
	}

	// A doc page renders the shared layout once, its own content, and marks
	// itself active in the sidebar.
	rec := get("/docs/library.html")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /docs/library.html = %d", rec.Code)
	}
	body := rec.Body.String()
	if n := strings.Count(body, `class="docs-nav"`); n != 1 {
		t.Errorf("sidebar rendered %d times, want exactly 1", n)
	}
	for _, want := range []string{
		`farfield · docs`,
		`<a href="library.html" aria-current="page">Library</a>`,
		`<a href="blobs.html">Blobs</a>`,
		`library.farfield.systems`, // page content
	} {
		if !strings.Contains(body, want) {
			t.Errorf("docs page missing %q", want)
		}
	}

	// The index renders at /docs/, and assets still serve.
	if rec := get("/docs/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Farfield Systems") {
		t.Errorf("GET /docs/ = %d", rec.Code)
	}
	if rec := get("/docs/style.css"); rec.Code != http.StatusOK {
		t.Errorf("GET /docs/style.css = %d", rec.Code)
	}
	// An unknown doc is a 404, not a raw template leak.
	if rec := get("/docs/nope.html"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /docs/nope.html = %d, want 404", rec.Code)
	}
}
