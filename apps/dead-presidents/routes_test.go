package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRoutesRegister is the guard for the failure that took this app down:
// a mangled route pattern ("GET /{" instead of "GET /{$}") passed go build,
// go vet, every other test, CI, and the Docker image build, then panicked at
// container start. Registration was only reachable through main(), so nothing
// ever ran it. Constructing the mux here is the whole point — if a pattern is
// invalid, ServeMux panics and this fails, on a laptop rather than in prod.
func TestRoutesRegister(t *testing.T) {
	site, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registering routes panicked: %v", r)
		}
	}()
	if routes(site) == nil {
		t.Fatal("routes returned nil")
	}
}

// TestRoutesServeTheShell walks the patterns the app actually depends on, so a
// route that registers but never matches is caught too.
func TestRoutesServeTheShell(t *testing.T) {
	site, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	mux := routes(site)

	for _, path := range []string{"/", "/index.html"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct == "" {
			t.Errorf("GET %s served no Content-Type", path)
		}
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /status = %d, want 200", rec.Code)
	}
}
