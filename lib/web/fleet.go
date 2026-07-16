package web

import (
	"html/template"
	"os"
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
// production subdomain, or the canonical localhost port under
// FARFIELD_FLEET=local.
func FleetBase(name string) string {
	if os.Getenv("FARFIELD_FLEET") == "local" {
		for _, a := range fleetApps {
			if a.Name == name {
				return "http://127.0.0.1:" + a.Port
			}
		}
	}
	return "https://" + name + ".farfield.systems"
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
		fleetHTML = template.HTML(b.String())
	})
	return fleetHTML
}
