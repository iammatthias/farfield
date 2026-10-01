package main

// apex's part of the ⌘K menu: every docs page and the fleet status page. All
// public, so /palette here needs no sign-in.

import (
	"net/http"

	"github.com/iammatthias/farfield/lib/web"
)

func paletteItems(r *http.Request) []web.PaletteItem {
	items := make([]web.PaletteItem, 0, len(docPages)+1)
	for _, d := range docPages {
		url, title := "/docs/"+d.Key, d.Label
		if d.Key == "index" {
			url, title = "/docs/", "Docs"
		}
		items = append(items, web.PaletteItem{Kind: "page", Title: title, Sub: "docs", URL: url,
			Words: "docs documentation"})
	}
	items = append(items, web.PaletteItem{Kind: "page", Title: "Status", Sub: "fleet", URL: "/status",
		Words: "up down health services"})
	return items
}
