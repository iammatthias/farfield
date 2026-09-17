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
	if string(body) != Styles {
		t.Fatal("plain body does not match theme.Styles")
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
	if string(body) != Styles {
		t.Fatal("gzip body does not round-trip to theme.Styles")
	}
	if len(w.Body.Bytes()) >= len(Styles) {
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

// TestFontSplitIsLossless: the stylesheet is served in two pieces now, and
// together they must still be the whole theme. A rule silently dropped by the
// splitter would show up as unstyled admin pages in production and nowhere
// else, so this checks the split preserves every byte that is not a face.
func TestFontSplitIsLossless(t *testing.T) {
	if strings.Contains(Styles, "@font-face") {
		t.Error("Styles still carries an @font-face block")
	}
	if !strings.Contains(Fonts, "@font-face") {
		t.Fatal("Fonts carries no @font-face block")
	}
	// Every declaration outside a face survives. Compare on non-blank lines so
	// the check does not hinge on how the splitter handles separators.
	lines := func(s string) map[string]int {
		out := map[string]int{}
		for _, ln := range strings.Split(s, "\n") {
			if ln = strings.TrimSpace(ln); ln != "" {
				out[ln]++
			}
		}
		return out
	}
	whole, split := lines(CSS), lines(Styles+"\n"+Fonts)
	for ln, n := range whole {
		if split[ln] != n {
			t.Errorf("line lost or duplicated by the split (%d -> %d): %.60s", n, split[ln], ln)
		}
	}
}

// TestFontsVersionIsIndependent: the whole point of the split is that editing
// a colour does not evict a quarter-megabyte of unchanged font bytes, which
// only holds while the two versions are computed from different inputs.
func TestFontsVersionIsIndependent(t *testing.T) {
	if FontsVersion == Version {
		t.Fatal("FontsVersion equals Version — the faces would ride the stylesheet's URL")
	}
	if strings.Contains(Styles, "data:font") {
		t.Error("a font data URI is still in Styles; the version split buys nothing")
	}
}
