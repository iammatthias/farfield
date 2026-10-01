package main

// content's part of the ⌘K menu: the things to start, and the entries,
// series and collections there are to open.

import (
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	items := []web.PaletteItem{
		{Kind: "action", Title: "New entry", URL: "/entries/new", Words: "write post draft compose"},
		{Kind: "action", Title: "New series", URL: "/series/new", Words: "gallery fragment"},
		{Kind: "action", Title: "New collection", URL: "/collections/new"},
		{Kind: "page", Title: "Search everything", URL: "/search", Words: "fleet find semantic"},
		{Kind: "page", Title: "Trash", URL: "/entries/trash", Words: "deleted restore"},
	}
	if cols, err := listCollections(s.db); err == nil {
		for _, c := range cols {
			items = append(items, web.PaletteItem{Kind: "record", Title: c.Name, Sub: "collection",
				URL: "/collections/" + c.Slug + "/edit", Words: c.Slug})
		}
	}
	if entries, err := listEntries(s.db, "", statusAll, web.PaletteLimit); err == nil {
		for _, e := range entries {
			sub := e.Collection
			if !e.Published {
				sub += " · draft"
			}
			title := e.Title
			if title == "" {
				title = e.Slug
			}
			items = append(items, web.PaletteItem{Kind: "record", Title: title, Sub: sub,
				URL: "/entries/" + e.Slug + "/edit", Words: strings.Join(e.Tags, " ")})
		}
	}
	if series, err := listSeries(s.db); err == nil {
		for i, sr := range series {
			if i == web.PaletteLimit {
				break
			}
			title := sr.Title
			if title == "" {
				title = sr.Slug
			}
			items = append(items, web.PaletteItem{Kind: "record", Title: title, Sub: "series",
				URL: "/series/" + sr.Slug + "/edit", Words: sr.Slug})
		}
	}
	return items
}
