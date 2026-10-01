package main

// qr's part of the ⌘K menu: the code to start, and the codes there are
// to edit.

import (
	"net/http"
	"net/url"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	items := []web.PaletteItem{
		{Kind: "action", Title: "New code", URL: "/new", Words: "qr create link"},
	}
	if codes, err := listCodes(s.db); err == nil {
		for i, c := range codes {
			if i == web.PaletteLimit {
				break
			}
			title := c.Label
			if title == "" {
				title = c.ID
			}
			sub := string(c.Mode)
			if c.Public {
				sub += " · public"
			} else {
				sub += " · private"
			}
			if !c.Enabled {
				sub += " · disabled"
			}
			words := c.ID
			if u, err := url.Parse(c.Target); err == nil && u.Hostname() != "" {
				words += " " + u.Hostname()
			}
			items = append(items, web.PaletteItem{Kind: "record", Title: title, Sub: sub,
				URL: "/codes/" + c.ID + "/edit", Words: words})
		}
	}
	return items
}
