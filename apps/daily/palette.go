package main

// daily's part of the ⌘K menu: the day's artifacts to open, and the
// photo archive's days. daily has no sign-in, so this list is public.

import (
	"net/http"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	items := []web.PaletteItem{
		{Kind: "page", Title: "Today's photo", URL: "/photo", Words: "apod nasa astronomy picture"},
		{Kind: "page", Title: "Photo archive", URL: "/photo/archive", Words: "apod nasa past days"},
		{Kind: "page", Title: "Today's art", URL: "/art", Words: "generative terrain plate"},
		{Kind: "page", Title: "Art structure", URL: "/art/structure", Words: "hilbert path"},
		{Kind: "page", Title: "Today's sudoku", URL: "/sudoku", Words: "puzzle"},
		{Kind: "page", Title: "Today's wordle", URL: "/wordle", Words: "word puzzle guess"},
	}
	if photos, err := listPhotos(s.db, sourceNASA, web.PaletteLimit, 0); err == nil {
		for _, p := range photos {
			if p.Placeholder {
				continue
			}
			title := p.Title
			if title == "" {
				title = p.Date
			}
			items = append(items, web.PaletteItem{Kind: "record", Title: title, Sub: p.Date + " · photo",
				URL: "/photo/" + p.Date, Words: p.Credit})
		}
	}
	return items
}
