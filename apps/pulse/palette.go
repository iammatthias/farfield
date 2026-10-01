package main

// pulse's part of the ⌘K menu: adding a target, and the monitored targets
// there are to open.

import (
	"net/http"
	"strconv"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	items := []web.PaletteItem{
		{Kind: "action", Title: "New target", URL: "/targets/new", Words: "monitor uptime check add"},
	}
	if targets, err := listTargets(s.db); err == nil {
		for i, t := range targets {
			if i == web.PaletteLimit {
				break
			}
			sub := t.URL
			if !t.Enabled {
				sub += " · paused"
			}
			items = append(items, web.PaletteItem{Kind: "record", Title: t.Name, Sub: sub,
				URL: "/targets/" + strconv.FormatInt(t.ID, 10) + "/edit", Words: "target monitor"})
		}
	}
	return items
}
