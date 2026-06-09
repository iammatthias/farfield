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
	rec := get("/docs/library")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /docs/library = %d", rec.Code)
	}
	body := rec.Body.String()
	if n := strings.Count(body, `class="docs-nav"`); n != 1 {
		t.Errorf("sidebar rendered %d times, want exactly 1", n)
	}
	for _, want := range []string{
		`farfield · docs`,
		`<a href="library" aria-current="page">Library</a>`,
		`<a href="blobs">Blobs</a>`,
		`library.farfield.systems`, // page content
	} {
		if !strings.Contains(body, want) {
			t.Errorf("docs page missing %q", want)
		}
	}

	// Pre-rendered pages carry cache validators, and a conditional GET on the
	// same ETag short-circuits to 304 with no body.
	etag := rec.Header().Get("ETag")
	if etag == "" || rec.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Errorf("docs page validators = ETag %q, Cache-Control %q", etag, rec.Header().Get("Cache-Control"))
	}
	req := httptest.NewRequest(http.MethodGet, "/docs/library", nil)
	req.Header.Set("If-None-Match", etag)
	cond := httptest.NewRecorder()
	h.ServeHTTP(cond, req)
	if cond.Code != http.StatusNotModified || cond.Body.Len() != 0 {
		t.Errorf("conditional GET = %d with %d body bytes, want 304 empty", cond.Code, cond.Body.Len())
	}

	// The legacy .html URLs 301 to the canonical extensionless form.
	if rec := get("/docs/library.html"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/docs/library" {
		t.Errorf("GET /docs/library.html = %d → %q, want 301 → /docs/library", rec.Code, rec.Header().Get("Location"))
	}
	if rec := get("/docs/index.html"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/docs/" {
		t.Errorf("GET /docs/index.html = %d → %q, want 301 → /docs/", rec.Code, rec.Header().Get("Location"))
	}

	// The index renders at /docs/, and assets still serve — versioned ones as
	// immutable, the rest with an hour of cache.
	if rec := get("/docs/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Farfield Systems") {
		t.Errorf("GET /docs/ = %d", rec.Code)
	}
	if rec := get("/docs/style.css?v=x"); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("GET /docs/style.css?v=x = %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := get("/docs/assets/apex.jpg"); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("GET /docs/assets/apex.jpg = %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := get("/docs/style.css"); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=3600" {
		t.Errorf("GET /docs/style.css = %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	// An unknown doc is a 404, not a raw template leak.
	if rec := get("/docs/nope.html"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /docs/nope.html = %d, want 404", rec.Code)
	}
}
