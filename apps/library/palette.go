package main

// library's part of the ⌘K menu: an upload to start, and the folders there
// are to open. Books have no page of their own, so they are not listed.

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	items := []web.PaletteItem{
		{Kind: "action", Title: "Upload", URL: "/upload", Words: "upload books epub"},
	}
	if named, _, err := collectionStats(s.db); err == nil {
		for i, c := range named {
			if i == web.PaletteLimit {
				break
			}
			sub := "folder · " + strconv.Itoa(c.Count) + " book"
			if c.Count != 1 {
				sub += "s"
			}
			items = append(items, web.PaletteItem{Kind: "record", Title: c.Name, Sub: sub,
				URL: "/?collection=" + url.QueryEscape(c.Name), Words: "collection"})
		}
	}
	return items
}
