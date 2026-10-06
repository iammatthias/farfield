package web

import (
	"encoding/json"
	"html/template"
	"os"
	"strconv"
	"strings"
	"sync"
)

// fleetApps is the constellation — every farfield service with a UI, in
// masthead order. Production URLs are the *.farfield.systems subdomains;
// FARFIELD_FLEET=local swaps in the canonical localhost ports (the devfleet
// script sets it) and FARFIELD_FLEET=off hides the menu.
var fleetApps = []struct {
	Name string
	Port string
}{
	{"content", "8787"},
	{"feed", "8788"},
	{"blobs", "8789"},
	{"library", "8797"},
	{"bookmarks", "8793"},
	{"daily", "8792"},
	{"qr", "8794"},
	{"scrap", "8799"},
	{"sideload", "8800"},
	{"keys", "8801"},
	{"pulse", "8798"},
	{"backup", "8791"},
}

var (
	fleetOnce sync.Once
	fleetHTML template.HTML
)

// FleetBase returns the browser-facing base URL for a fleet app — the
// production subdomain, or the canonical local port under
// FARFIELD_FLEET=local. FARFIELD_FLEET_HOST names the host those ports are
// reached on (default 127.0.0.1) — a tailnet name when a local fleet is
// previewed from another device.
//
// FARFIELD_URL_<NAME> (FARFIELD_URL_BACKUP) overrides one app's base: backup
// is tailnet-only in production, reached at its tailscale serve address, and
// that address is the deployment's to know, not the repo's.
func FleetBase(name string) string {
	if u := os.Getenv("FARFIELD_URL_" + strings.ToUpper(name)); u != "" {
		return strings.TrimRight(u, "/")
	}
	if os.Getenv("FARFIELD_FLEET") == "local" {
		host := os.Getenv("FARFIELD_FLEET_HOST")
		if host == "" {
			host = "127.0.0.1"
		}
		for _, a := range fleetApps {
			if a.Name == name {
				return "http://" + host + ":" + a.Port
			}
		}
	}
	return "https://" + name + "." + canonicalDomain
}

// fleetNav renders the cross-app switcher menu. Renderer.Render injects it
// into every page as .FleetNav; admin mastheads include it with
// {{.FleetNav}}. Public pages simply don't reference it.
func fleetNav() template.HTML {
	fleetOnce.Do(func() {
		mode := os.Getenv("FARFIELD_FLEET")
		if mode == "off" {
			return
		}
		var b strings.Builder
		b.WriteString(`<details class="fleet"><summary>fleet</summary><nav>`)
		b.WriteString(`<a class="fleet-wide" href="` + FleetBase("content") + `/search">search the fleet</a>`)
		for _, a := range fleetApps {
			b.WriteString(`<a href="` + FleetBase(a.Name) + `">` + a.Name + `</a>`)
		}
		b.WriteString(`</nav></details>`)
		// The menu is rendered once for every request, so its links are on the
		// canonical domain. Seen from another fleet domain (a console reached
		// on *.iam.casa), move them onto that one — the session cookie there
		// does not reach farfield.systems. The palette does the same server
		// side, where it has the request (RebaseFleetURL).
		if ds := fleetDomains(); len(ds) > 1 {
			list, _ := json.Marshal(ds)
			b.WriteString(`<script>(function(){var c=` + strconv.Quote(canonicalDomain) + `,h=location.hostname,` +
				`t=` + string(list) + `.filter(function(d){return h===d||h.endsWith("."+d)})[0];` +
				`if(!t||t===c)return;document.currentScript.previousElementSibling.querySelectorAll("a").forEach(function(a){` +
				`var u=new URL(a.href);if(u.hostname.endsWith("."+c)){u.hostname=u.hostname.slice(0,-c.length)+t;a.href=u.href}})})()</script>`)
		}
		fleetHTML = template.HTML(b.String())
	})
	return fleetHTML
}
