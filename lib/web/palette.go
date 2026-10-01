package web

// The ⌘K menu's data. Every app answers GET /palette with what it can do
// (actions), where you can go (its pages) and what it holds (records), and
// the menu (lib/theme/palette.js) gathers every app's answer into one list.
//
// Fleet-wide on purpose: the apps are one ecosystem, and the signed fleet
// session (SESSION_SECRET + SESSION_COOKIE_DOMAIN) means one login reaches all
// of them — so a menu opened on any app fetches the others' /palette with that
// same cookie. CORS answers only fleet origins, with credentials; anything
// else gets no CORS headers and the browser refuses to share the response.

import (
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/iammatthias/farfield/lib/theme"
)

// PaletteItem is one thing the menu can find.
type PaletteItem struct {
	Kind  string `json:"kind"`          // "action", "page" or "record"
	Title string `json:"title"`         // what it is called
	Sub   string `json:"sub,omitempty"` // a line of context: a date, a slug
	URL   string `json:"url"`           // where it goes (relative to the app)
	Words string `json:"words,omitempty"`
}

// PaletteSource lists an app's own actions and records for a signed-in
// request. Pages come from the Renderer's Nav and need no listing. Keep it to
// the recent few hundred records: the menu ranks in the browser.
type PaletteSource func(r *http.Request) []PaletteItem

// PaletteLimit is how many records of one kind a source should list.
const PaletteLimit = 200

func local() bool { return os.Getenv("FARFIELD_FLEET") == "local" }

func fleetHost() string {
	if h := os.Getenv("FARFIELD_FLEET_HOST"); h != "" {
		return h
	}
	return "127.0.0.1"
}

// paletteFleet is every app the menu reaches: the masthead's fleet plus apex
// (the site root, not a subdomain) and switchboard. backup is tailnet-only in
// production, where a browser on the public fleet cannot reach it.
func paletteFleet() []map[string]string {
	out := []map[string]string{}
	for _, a := range fleetApps {
		if a.Name == "backup" && !local() {
			continue
		}
		out = append(out, map[string]string{"name": a.Name, "url": FleetBase(a.Name)})
	}
	out = append(out, map[string]string{"name": "switchboard", "url": paletteBase("switchboard")})
	out = append(out, map[string]string{"name": "apex", "url": paletteBase("apex")})
	return out
}

// paletteBase is the browser-facing base URL for an app.
func paletteBase(name string) string {
	switch {
	case name == "apex" && local():
		return "http://" + fleetHost() + ":8790"
	case name == "apex":
		return "https://farfield.systems"
	case name == "switchboard" && local():
		return "http://" + fleetHost() + ":8802"
	}
	return FleetBase(name)
}

// fleetOrigin reports whether an Origin header is one of the fleet's own:
// farfield.systems and its subdomains over https, or — for a local preview —
// any port on the preview host.
func fleetOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if u.Scheme == "https" && (host == "farfield.systems" || strings.HasSuffix(host, ".farfield.systems")) {
		return true
	}
	if local() && u.Scheme == "http" {
		return host == fleetHost() || host == "127.0.0.1" || host == "localhost"
	}
	return false
}

// PaletteCORS lets another fleet app's page read a response with the user's
// cookie, and answers its preflight. Origins outside the fleet get nothing.
// Call it before any session check: a preflight carries no cookie.
func PaletteCORS(w http.ResponseWriter, r *http.Request) (preflight bool) {
	origin := r.Header.Get("Origin")
	w.Header().Add("Vary", "Origin")
	if origin != "" && fleetOrigin(origin) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "600")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// MountPalette serves the menu for an app: GET /palette (its items) and
// /static/palette.js, and turns the script on in the masthead. a may be nil
// for a public app with no sign-in: its /palette still lists its pages for
// the other apps' menus, but its own pages — read by the public — carry no
// menu. src may be nil for an app whose pages are all there is.
func (rd *Renderer) MountPalette(mux *http.ServeMux, a *Auth, src PaletteSource) {
	rd.palette = a != nil
	handler := func(w http.ResponseWriter, r *http.Request) {
		if PaletteCORS(w, r) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		app := rd.App
		if app == "" {
			app = "farfield"
		}
		out := map[string]any{"app": app, "base": paletteBase(app), "fleet": paletteFleet()}

		signedIn := a == nil || a.SessionValid(r)
		out["signedIn"] = signedIn
		if !signedIn {
			// enough to offer the way in, nothing behind the login
			out["items"] = []PaletteItem{{Kind: "action", Title: "Sign in to " + app, URL: "/login"}}
			WriteJSON(w, http.StatusOK, out)
			return
		}
		var own []PaletteItem
		if src != nil {
			own = src(r)
		}
		// a source item at a Nav page's URL (an app's "New post" is often in
		// its masthead too) says more than the bare link: it replaces it
		listed := map[string]bool{}
		for _, it := range own {
			listed[it.URL] = true
		}
		items := []PaletteItem{{Kind: "page", Title: app, Sub: "home", URL: "/"}}
		for _, n := range rd.Nav {
			if n.URL == "" || n.URL == "/" || listed[n.URL] || strings.Contains(strings.ToLower(n.Label), "log out") {
				continue
			}
			items = append(items, PaletteItem{Kind: "page", Title: n.Label, URL: n.URL})
		}
		items = append(items, own...)
		out["items"] = items
		WriteJSON(w, http.StatusOK, out)
	}
	mux.HandleFunc("GET /palette", handler)
	mux.HandleFunc("OPTIONS /palette", handler)
	mux.HandleFunc("GET /static/palette.js", theme.PaletteJSHandler())
}

// RequireFleetSession gates an endpoint the menu calls from any fleet app:
// the preflight is answered first (it carries no cookie), the Origin must be
// the fleet's — the cross-origin form of RequireSession's CSRF check — and a
// missing session is a JSON 401, not a redirect a fetch would follow.
func (a *Auth) RequireFleetSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if PaletteCORS(w, r) {
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !fleetOrigin(o) && !allowedOrigin(r) {
			WriteError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		if !a.SessionValid(r) {
			WriteError(w, http.StatusUnauthorized, "sign in first")
			return
		}
		next(w, r)
	}
}
