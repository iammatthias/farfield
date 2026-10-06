package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/auth"
)

// With FARFIELD_LOGIN_URL set, a console with no session sends the browser
// to the fleet login carrying the absolute URL it wanted; without it, the
// app's own /login exactly as before.
func TestRequireSessionFleetLoginURL(t *testing.T) {
	fleetMode(t)
	a := &Auth{}
	h := a.RequireSession(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler ran without a session")
	})
	get := func(method string) string {
		r := httptest.NewRequest(method, "/entries/x?tab=meta", nil)
		r.Host = "content.farfield.systems"
		r.RemoteAddr = "172.18.0.5:40000" // the proxy on the container network
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s status = %d, want 303", method, w.Code)
		}
		return w.Header().Get("Location")
	}

	if loc := get("GET"); loc != "/login" {
		t.Errorf("unset FARFIELD_LOGIN_URL: Location = %q, want /login", loc)
	}

	t.Setenv("FARFIELD_LOGIN_URL", "https://keys.farfield.systems/login")
	loc := get("GET")
	u, err := url.Parse(loc)
	if err != nil || u.Host != "keys.farfield.systems" || u.Path != "/login" {
		t.Fatalf("Location = %q, want the keys login", loc)
	}
	if next := u.Query().Get("next"); next != "https://content.farfield.systems/entries/x?tab=meta" {
		t.Errorf("next = %q, want the absolute URL of the request", next)
	}
	// A write cannot be replayed as a GET after login, so it carries no next.
	if loc := get("POST"); loc != "https://keys.farfield.systems/login" {
		t.Errorf("POST Location = %q, want the bare login URL", loc)
	}
}

func TestValidNext(t *testing.T) {
	fleetMode(t)
	cookieDomain = ".farfield.systems"

	prod := httptest.NewRequest("GET", "/login", nil)
	prod.Host = "keys.farfield.systems"
	dev := httptest.NewRequest("GET", "/login", nil)
	dev.Host = "localhost:8801"

	for _, tc := range []struct {
		r    *http.Request
		next string
		want bool
	}{
		{prod, "/passkeys", true},
		{prod, "/device/authorize?client_id=farfield-desktop", true},
		{prod, "https://keys.farfield.systems/passkeys", true},
		{prod, "https://content.farfield.systems/entries/x", true},
		{prod, "https://farfield.systems/", true},
		{prod, "//evil.example/", false},
		{prod, "/\\evil.example", false},
		{prod, "https://evil.example/", false},
		{prod, "https://farfield.systems.evil.example/", false},
		{prod, "https://evilfarfield.systems/", false},
		{prod, "https://user@content.farfield.systems/", false},
		{prod, "javascript:alert(1)", false},
		{prod, "http://127.0.0.1:8787/", false}, // production never bounces to loopback
		{prod, "", false},
		{dev, "http://127.0.0.1:8787/", true},
		{dev, "http://localhost:8787/entries", true},
		{dev, "https://evil.example/", false},
	} {
		if got := ValidNext(tc.r, tc.next); got != tc.want {
			t.Errorf("ValidNext(host %s, %q) = %v, want %v", tc.r.Host, tc.next, got, tc.want)
		}
	}
}

func TestHandleLoginHonorsNext(t *testing.T) {
	fleetMode(t)
	cookieDomain = ".farfield.systems"
	a := &Auth{Password: "pw"}
	login := func(password, next string) *httptest.ResponseRecorder {
		form := url.Values{"password": {password}, "next": {next}}
		r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Host = "keys.farfield.systems"
		r.RemoteAddr = "198.51.100.30:1"
		w := httptest.NewRecorder()
		a.HandleLogin(w, r)
		return w
	}
	if loc := login("pw", "https://content.farfield.systems/x").Header().Get("Location"); loc != "https://content.farfield.systems/x" {
		t.Errorf("fleet next: Location = %q", loc)
	}
	if loc := login("pw", "https://evil.example/").Header().Get("Location"); loc != "/" {
		t.Errorf("foreign next: Location = %q, want /", loc)
	}
	// A wrong password keeps next, so the retry still lands in the right place.
	loc := login("nope", "/passkeys").Header().Get("Location")
	u, _ := url.Parse(loc)
	if u.Path != "/login" || u.Query().Get("next") != "/passkeys" || u.Query().Get("error") == "" {
		t.Errorf("failed login Location = %q, want /login with error and next", loc)
	}
}

func TestSessionFresh(t *testing.T) {
	t.Run("fleet", func(t *testing.T) {
		fleetMode(t)
		a := &Auth{}
		req := func(token string) *http.Request {
			r := httptest.NewRequest("GET", "/", nil)
			r.AddCookie(&http.Cookie{Name: "session", Value: token})
			return r
		}
		exp := time.Now().Add(time.Hour)
		fresh := auth.SignSession(fleetSecret, "", exp)
		stale := auth.SignSessionAt(fleetSecret, "", time.Now().Add(-10*time.Minute), exp)
		if !a.SessionFresh(req(fresh), 5*time.Minute) {
			t.Error("a just-minted session is not fresh")
		}
		if a.SessionFresh(req(stale), 5*time.Minute) {
			t.Error("a ten-minute-old session is fresh")
		}
		if !a.SessionValid(req(stale)) {
			t.Error("a stale session must still be a valid session")
		}
		if a.SessionFresh(httptest.NewRequest("GET", "/", nil), time.Hour) {
			t.Error("no session is fresh")
		}
	})
}

func TestPrivateIngress(t *testing.T) {
	h := PrivateIngress(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	})
	for name, hdr := range map[string]string{"Cf-Ray": "x", "Cf-Connecting-Ip": "203.0.113.1"} {
		r := httptest.NewRequest("POST", "/device/token", nil)
		r.Header.Set(name, hdr)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("POST", "/device/token", nil))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("private request: %d, Cache-Control %q; want 200, no-store",
			w.Code, w.Header().Get("Cache-Control"))
	}
}
