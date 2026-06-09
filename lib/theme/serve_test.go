package theme

import (
	"compress/gzip"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCSSHandlerHeaders(t *testing.T) {
	h := CSSHandler()
	r := httptest.NewRequest("GET", "/static/styles.css?v="+Version, nil)
	w := httptest.NewRecorder()
	h(w, r)

	res := w.Result()
	if got := res.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("Cache-Control = %q, want immutable", got)
	}
	if res.Header.Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != CSS {
		t.Fatal("plain body does not match theme.CSS")
	}
}

func TestCSSHandlerGzip(t *testing.T) {
	h := CSSHandler()
	r := httptest.NewRequest("GET", "/static/styles.css", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h(w, r)

	res := w.Result()
	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if string(body) != CSS {
		t.Fatal("gzip body does not round-trip to theme.CSS")
	}
	if len(w.Body.Bytes()) >= len(CSS) {
		t.Fatal("gzip variant is not smaller than the source")
	}
}

func TestCSSHandlerNotModified(t *testing.T) {
	h := CSSHandler()
	r := httptest.NewRequest("GET", "/static/styles.css", nil)
	r.Header.Set("If-None-Match", `"`+Version+`"`)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != 304 {
		t.Fatalf("status = %d, want 304", w.Code)
	}
}

func TestVersionShape(t *testing.T) {
	if len(Version) != 16 {
		t.Fatalf("Version length = %d, want 16", len(Version))
	}
}
