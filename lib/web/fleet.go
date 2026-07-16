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
		for _, a := range fleetApps {
			href := "https://" + a.Name + ".farfield.systems"
			if mode == "local" {
				href = "http://127.0.0.1:" + a.Port
			}
			b.WriteString(`<a href="` + href + `">` + a.Name + `</a>`)
		}
		b.WriteString(`</nav></details>`)
		fleetHTML = template.HTML(b.String())
	})
	return fleetHTML
}
