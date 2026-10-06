package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// twoDomains is production: the fleet on farfield.systems through the
// tunnel and on iam.casa over the tailnet.
func twoDomains(t *testing.T) {
	t.Helper()
	fleetMode(t)
	cookieDomain = ".farfield.systems, .iam.casa"
}

func reqOn(method, host, path string) *http.Request {
	r := httptest.NewRequest(method, "https://"+host+path, nil)
	r.Host = host
	return r
}

func TestFleetDomainOf(t *testing.T) {
	twoDomains(t)
	for host, want := range map[string]string{
		"content.farfield.systems":   "farfield.systems",
		"farfield.systems":           "farfield.systems",
		"keys.iam.casa":              "iam.casa",
		"IAM.CASA.":                  "iam.casa",
		"homelab.tailcf0ef1.ts.net":  "",
		"evilfarfield.systems":       "",
		"iam.casa.evil.example":      "",
		"notiam.casa":                "",
		"farfield.systems.iam.casa2": "",
	} {
		if got := fleetDomainOf(host); got != want {
			t.Errorf("fleetDomainOf(%q) = %q, want %q", host, got, want)
		}
	}
}

// The session cookie is scoped to the domain the login happened on: a fleet
// cookie for .farfield.systems set on an iam.casa host would be rejected by
// the browser, and the login would loop.
func TestSessionCookieFollowsRequestDomain(t *testing.T) {
	twoDomains(t)
	a := &Auth{}
	for host, want := range map[string]string{
		"keys.farfield.systems":     ".farfield.systems",
		"keys.iam.casa":             ".iam.casa",
		"homelab.tailcf0ef1.ts.net": "", // host-only: no fleet domain reaches it
	} {
		w := httptest.NewRecorder()
		if err := a.OpenSession(w, reqOn("POST", host, "/login")); err != nil {
			t.Fatal(err)
		}
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("%s: %d cookies", host, len(cookies))
		}
		// net/http drops the leading dot when it parses Domain back.
		if got := cookies[0].Domain; "."+got != want && got != want {
			t.Errorf("%s: cookie Domain = %q, want %q", host, got, want)
		}
	}
}

func TestLoginRedirectFollowsRequestDomain(t *testing.T) {
	twoDomains(t)
	t.Setenv("FARFIELD_LOGIN_URL", "https://keys.farfield.systems/login")
	a := &Auth{}
	h := a.RequireSession(func(http.ResponseWriter, *http.Request) {})

	loc := func(host string) string {
		w := httptest.NewRecorder()
		h(w, reqOn("GET", host, "/entries"))
		return w.Header().Get("Location")
	}

	u, _ := url.Parse(loc("content.iam.casa"))
	if u.Host != "keys.iam.casa" || u.Query().Get("next") != "https://content.iam.casa/entries" {
		t.Errorf("tailnet login = %s, want keys.iam.casa with next on content.iam.casa", u)
	}
	if u, _ := url.Parse(loc("content.farfield.systems")); u.Host != "keys.farfield.systems" {
		t.Errorf("public login = %s, want keys.farfield.systems", u)
	}
	if got := loc("homelab.tailcf0ef1.ts.net:8787"); got != "/login" {
		t.Errorf("off-fleet host login = %q, want the app's own /login", got)
	}
}

// A login on one domain cannot open a session on the other, so it must not
// redirect there either — and a write from the other domain is cross-site.
func TestNextAndOriginStayOnOneDomain(t *testing.T) {
	twoDomains(t)
	keys := reqOn("GET", "keys.iam.casa", "/login")
	for next, want := range map[string]bool{
		"https://content.iam.casa/entries":         true,
		"https://iam.casa/":                        true,
		"https://content.farfield.systems/entries": false,
		"https://evil.example/":                    false,
	} {
		if got := ValidNext(keys, next); got != want {
			t.Errorf("ValidNext(keys.iam.casa, %q) = %v, want %v", next, got, want)
		}
	}

	for origin, want := range map[string]bool{
		"https://content.iam.casa":         true,
		"https://content.farfield.systems": false,
		"https://evil.example":             false,
	} {
		r := reqOn("POST", "blobs.iam.casa", "/upload")
		r.Header.Set("Origin", origin)
		if got := allowedOrigin(r); got != want {
			t.Errorf("allowedOrigin(blobs.iam.casa, Origin %s) = %v, want %v", origin, got, want)
		}
	}
}

func TestRebaseFleetURL(t *testing.T) {
	twoDomains(t)
	tail := reqOn("GET", "content.iam.casa", "/")
	pub := reqOn("GET", "content.farfield.systems", "/")
	off := reqOn("GET", "homelab.tailcf0ef1.ts.net:8787", "/")
	for _, tc := range []struct {
		r        *http.Request
		in, want string
	}{
		{tail, "https://feed.farfield.systems/posts/x", "https://feed.iam.casa/posts/x"},
		{tail, "https://feed.farfield.systems:8443/x", "https://feed.iam.casa:8443/x"},
		{tail, "https://homelab.tailcf0ef1.ts.net:8791", "https://homelab.tailcf0ef1.ts.net:8791"},
		{tail, "/relative", "/relative"},
		{pub, "https://feed.farfield.systems/", "https://feed.farfield.systems/"},
		{pub, "https://feed.iam.casa/", "https://feed.farfield.systems/"},
		{off, "https://feed.farfield.systems/", "https://feed.farfield.systems/"},
	} {
		if got := RebaseFleetURL(tc.r, tc.in); got != tc.want {
			t.Errorf("RebaseFleetURL(%s, %s) = %s, want %s", tc.r.Host, tc.in, got, tc.want)
		}
	}
}

func TestPaletteFollowsRequestDomain(t *testing.T) {
	twoDomains(t)
	for _, f := range paletteFleet(reqOn("GET", "content.iam.casa", "/palette")) {
		switch f["name"] {
		case "apex":
			if f["url"] != "https://farfield.systems" {
				t.Errorf("apex = %s, want the public site", f["url"])
			}
		case "backup":
		default:
			if !strings.HasSuffix(f["url"], ".iam.casa") {
				t.Errorf("%s = %s, want an iam.casa URL", f["name"], f["url"])
			}
		}
	}
	if !fleetOrigin("https://feed.iam.casa") || fleetOrigin("http://feed.iam.casa") {
		t.Error("fleetOrigin: want https iam.casa siblings only")
	}
}
