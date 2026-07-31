package web

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Origin returns the public scheme://host this request arrived on — the base
// every absolute URL an app publishes is built from.
//
// The forwarded scheme is believed ONLY from a trusted peer, the same rule
// ClientIP applies and for the same reason: these values end up in robots.txt
// and sitemap.xml, so a client that can reach the app directly must not be
// able to dictate the URLs we hand to crawlers. Untrusted peers fall back to
// whether the connection itself was TLS.
func Origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if trustedPeer(peerIP(r)) {
		if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
			// A proxy chain appends, so the first hop is the client-facing one.
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			if v = strings.TrimSpace(v); v == "http" || v == "https" {
				scheme = v
			}
		}
	}
	return scheme + "://" + r.Host
}

// crawlerDisallow are the paths no crawler benefits from. They are already
// session-gated, so this is not a security control — it spares crawlers a pile
// of 303s to a login page and keeps the redirects out of search results.
var crawlerDisallow = []string{"/login", "/logout", "/api/"}

// RobotsHandler serves a permissive robots.txt naming the app's sitemap.
// sitemapPath may be empty for apps that publish no sitemap.
//
// Every farfield host needs its own: crawlers fetch /robots.txt per hostname,
// and without an origin file the CDN synthesises one that carries no Sitemap:
// line. Serving our own also lets the CDN append its content-signal policy to
// real directives rather than standing in for them.
func RobotsHandler(sitemapPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("User-agent: *\n")
		for _, p := range crawlerDisallow {
			fmt.Fprintf(&b, "Disallow: %s\n", p)
		}
		b.WriteString("Allow: /\n")
		if sitemapPath != "" {
			fmt.Fprintf(&b, "\nSitemap: %s%s\n", Origin(r), sitemapPath)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = io.WriteString(w, b.String())
	}
}

// SitemapHandler serves a sitemap listing paths, resolved against the origin
// the request arrived on so the same binary is correct in dev and behind the
// tunnel. Paths are root-relative ("/", "/docs/"); anything else is skipped
// rather than emitted as a malformed <loc>.
func SitemapHandler(paths ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := Origin(r)
		var b bytes.Buffer
		b.WriteString(xml.Header)
		b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
		for _, p := range paths {
			if !strings.HasPrefix(p, "/") {
				continue
			}
			b.WriteString("  <url><loc>")
			// The loc is attacker-influenced through Host, so escape it rather
			// than trusting it to be XML-safe.
			_ = xml.EscapeText(&b, []byte(origin+p))
			b.WriteString("</loc></url>\n")
		}
		b.WriteString("</urlset>\n")
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = b.WriteTo(w)
	}
}
