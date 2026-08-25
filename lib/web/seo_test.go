package web

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOriginUsesForwardedProtoFromTrustedPeer(t *testing.T) {
	r := httptest.NewRequest("GET", "/robots.txt", nil)
	r.Host = "daily.farfield.systems"
	r.RemoteAddr = "172.17.0.1:5555" // the docker bridge — private, trusted
	r.Header.Set("X-Forwarded-Proto", "https")

	if got, want := Origin(r), "https://daily.farfield.systems"; got != want {
		t.Fatalf("Origin = %q, want %q", got, want)
	}
}

// The forwarded scheme decides what we publish to crawlers, so it must not be
// believed from a peer that reached the app directly.
func TestOriginIgnoresForwardedProtoFromUntrustedPeer(t *testing.T) {
	r := httptest.NewRequest("GET", "/robots.txt", nil)
	r.Host = "daily.farfield.systems"
	r.RemoteAddr = "203.0.113.9:5555" // public address, not a proxy we trust
	r.Header.Set("X-Forwarded-Proto", "https")

	if got, want := Origin(r), "http://daily.farfield.systems"; got != want {
		t.Fatalf("Origin = %q, want %q — a public peer must not set the scheme", got, want)
	}
}

func TestOriginUsesTLSWhenTheConnectionIsDirect(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "example.test"
	r.RemoteAddr = "203.0.113.9:5555"
	r.TLS = &tls.ConnectionState{}

	if got, want := Origin(r), "https://example.test"; got != want {
		t.Fatalf("Origin = %q, want %q", got, want)
	}
}

func TestRobotsHandlerNamesTheSitemapAndAllowsCrawling(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/robots.txt", nil)
	r.Host = "farfield.systems"
	r.RemoteAddr = "172.17.0.1:5555"
	r.Header.Set("X-Forwarded-Proto", "https")

	RobotsHandler("/sitemap.xml")(w, r)

	body := w.Body.String()
	for _, want := range []string{
		"User-agent: *",
		"Allow: /",
		"Disallow: /login",
		"Sitemap: https://farfield.systems/sitemap.xml",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("robots.txt missing %q:\n%s", want, body)
		}
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

func TestRobotsHandlerOmitsSitemapLineWhenThereIsNone(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/robots.txt", nil)
	r.Host = "library.farfield.systems"

	RobotsHandler("")(w, r)

	if strings.Contains(w.Body.String(), "Sitemap:") {
		t.Errorf("expected no Sitemap: line:\n%s", w.Body.String())
	}
}

func TestSitemapHandlerEmitsAbsoluteURLs(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/sitemap.xml", nil)
	r.Host = "farfield.systems"
	r.RemoteAddr = "172.17.0.1:5555"
	r.Header.Set("X-Forwarded-Proto", "https")

	SitemapHandler("/", "/docs/", "/docs/daily")(w, r)

	body := w.Body.String()
	for _, want := range []string{
		"<loc>https://farfield.systems/</loc>",
		"<loc>https://farfield.systems/docs/</loc>",
		"<loc>https://farfield.systems/docs/daily</loc>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sitemap missing %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, "<?xml") {
		t.Errorf("sitemap should start with an XML declaration:\n%s", body)
	}
}

// Host reaches us as a header, so it must not be able to break out of <loc>.
func TestSitemapHandlerEscapesTheHost(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/sitemap.xml", nil)
	r.Host = "evil.test/<script>"

	SitemapHandler("/")(w, r)

	if strings.Contains(w.Body.String(), "<script>") {
		t.Errorf("host was not escaped into the sitemap:\n%s", w.Body.String())
	}
}

func TestSitemapHandlerSkipsNonRootRelativePaths(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/sitemap.xml", nil)
	r.Host = "farfield.systems"

	SitemapHandler("/", "docs/", "https://elsewhere.test/x")(w, r)

	if n := strings.Count(w.Body.String(), "<url>"); n != 1 {
		t.Errorf("got %d <url> entries, want 1 — only root-relative paths belong", n)
	}
}

func TestRobotsAndSitemapAreServeableFromAMux(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /robots.txt", RobotsHandler("/sitemap.xml"))
	mux.HandleFunc("GET /sitemap.xml", SitemapHandler("/"))

	for _, path := range []string{"/robots.txt", "/sitemap.xml"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, w.Code)
		}
	}
}
