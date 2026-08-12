package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// browserGet issues a GET with a browser-shaped Accept header.
func browserGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestNotFoundPlateReplacesDefault404: a mux miss under a browser Accept
// header renders the plate page, not the stock text/plain body.
func TestNotFoundPlateReplacesDefault404(t *testing.T) {
	h := NotFoundPlate(http.NewServeMux())
	w := browserGet(t, h, "/no/such/page")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Not in the catalogue") {
		t.Fatal("plate page not rendered")
	}
	if !strings.Contains(body, plate404Path+"?v="+plate404Ver) {
		t.Fatal("page does not link the versioned artwork")
	}
	if strings.Contains(body, "404 page not found") {
		t.Fatal("stock body leaked into the plate page")
	}
}

// TestNotFoundPlateLeavesAPIsAlone: a JSON 404 and a curl-shaped request keep
// their original bytes; so does a handler's own HTML 404 and any success.
func TestNotFoundPlateLeavesAPIsAlone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/thing", func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusNotFound, "no such thing")
	})
	mux.HandleFunc("GET /custom", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<h1>our own 404</h1>")
	})
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "fine")
	})
	h := NotFoundPlate(mux)

	// JSON API 404 — Content-Type wins over the Accept header.
	if w := browserGet(t, h, "/api/thing"); !strings.Contains(w.Body.String(), "no such thing") {
		t.Fatalf("JSON 404 was replaced: %q", w.Body.String())
	}

	// A handler's own HTML 404 is its own business.
	if w := browserGet(t, h, "/custom"); !strings.Contains(w.Body.String(), "our own 404") {
		t.Fatalf("custom HTML 404 was replaced: %q", w.Body.String())
	}

	// curl sends Accept: */* — no HTML, no plate.
	r := httptest.NewRequest("GET", "/no/such/page", nil)
	r.Header.Set("Accept", "*/*")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "404 page not found") {
		t.Fatalf("non-browser 404 was replaced: %q", w.Body.String())
	}

	// Success bodies pass through untouched.
	if w := browserGet(t, h, "/ok"); w.Body.String() != "fine" {
		t.Fatalf("200 body altered: %q", w.Body.String())
	}
}

// TestNotFoundPlateThroughGzip: apps hand Serve a Gzip-wrapped handler, so
// the interception happens outside a layer that has already promised
// Content-Encoding: gzip. The replaced page must arrive uncompressed and
// uncorrupted.
func TestNotFoundPlateThroughGzip(t *testing.T) {
	h := NotFoundPlate(LogRequests(Gzip(http.NewServeMux())))
	r := httptest.NewRequest("GET", "/no/such/page", nil)
	r.Header.Set("Accept", "text/html")
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if enc := w.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q, want none", enc)
	}
	if !strings.Contains(w.Body.String(), "Not in the catalogue") {
		t.Fatal("plate page corrupted through the gzip chain")
	}
	if bytes.HasPrefix(w.Body.Bytes(), []byte{0x1f, 0x8b}) {
		t.Fatal("body is gzip bytes despite no Content-Encoding")
	}
	// And a compressed success still round-trips.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, strings.Repeat("compressible ", 100))
	})
	h = NotFoundPlate(Gzip(mux))
	r = httptest.NewRequest("GET", "/ok", nil)
	r.Header.Set("Accept", "text/html")
	r.Header.Set("Accept-Encoding", "gzip")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("success lost its compression")
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil || !strings.Contains(string(out), "compressible") {
		t.Fatalf("compressed success corrupted (err %v)", err)
	}
}

// TestNotFoundPlateServesArt: the reserved artwork path answers from the
// embedded bytes with content-addressed caching, in every app.
func TestNotFoundPlateServesArt(t *testing.T) {
	h := NotFoundPlate(http.NewServeMux())

	r := httptest.NewRequest("GET", plate404Path+"?v="+plate404Ver, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), plate404) {
		t.Fatalf("artwork not served (status %d, %d bytes)", w.Code, w.Body.Len())
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("versioned artwork Cache-Control = %q, want immutable", cc)
	}

	// Revalidation by ETag.
	r = httptest.NewRequest("GET", plate404Path, nil)
	r.Header.Set("If-None-Match", `"`+plate404Ver+`"`)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified {
		t.Fatalf("ETag revalidation: status = %d, want 304", w.Code)
	}
}
