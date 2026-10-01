package main

// blobs' part of the ⌘K menu: an upload to start, and the recent blobs —
// each opening in its viewer on the admin page that lists it.

import (
	"net/http"
	"strconv"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	items := []web.PaletteItem{
		{Kind: "action", Title: "Upload", URL: "/upload", Words: "upload blob file image media"},
	}
	// Same query and order as the index, so i/pageSize is the page it is on.
	if blobs, err := listMeta(s.db, web.PaletteLimit, 0); err == nil {
		for i, m := range blobs {
			title := m.CID
			if len(title) > 24 {
				title = title[:24] + "…"
			}
			sub := m.Mime
			if m.Width > 0 {
				sub += " · " + strconv.Itoa(m.Width) + "×" + strconv.Itoa(m.Height)
			}
			if len(m.CreatedAt) >= 10 {
				sub += " · " + m.CreatedAt[:10]
			}
			items = append(items, web.PaletteItem{Kind: "record", Title: title, Sub: sub,
				URL: "/?page=" + strconv.Itoa(i/pageSize+1) + "#m-" + m.CID, Words: m.CID + " " + m.Mime})
		}
	}
	return items
}
