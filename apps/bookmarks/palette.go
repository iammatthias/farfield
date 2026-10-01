package main

// bookmarks' part of the ⌘K menu: a new bookmark to save, and the recent
// bookmarks there are to open.

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	items := []web.PaletteItem{
		{Kind: "action", Title: "New bookmark", URL: "/new", Words: "save link url"},
	}
	if bs, err := listBookmarks(s.db); err == nil {
		for i, b := range bs {
			if i == web.PaletteLimit {
				break
			}
			title := b.Title
			if title == "" {
				title = b.URL
			}
			sub := b.Category
			if !b.Public {
				sub = strings.TrimPrefix(sub+" · private", " · ")
			}
			host := ""
			if u, err := url.Parse(b.URL); err == nil {
				host = u.Hostname()
			}
			items = append(items, web.PaletteItem{Kind: "record", Title: title, Sub: sub,
				URL: "/bookmarks/" + b.ID + "/edit", Words: strings.TrimSpace(host + " " + b.Category + " " + b.OGSiteName)})
		}
	}
	return items
}
