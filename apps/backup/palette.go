package main

// backup's part of the ⌘K menu: taking a snapshot. Snapshots have no page of
// their own (only a delete), so there are no records to open.

import (
	"net/http"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	// The snapshot itself is a POST; the menu navigates, so it opens the
	// dashboard that holds the "Snapshot now" button.
	return []web.PaletteItem{
		{Kind: "action", Title: "Snapshot now", URL: "/", Words: "backup take r2 snapshots"},
	}
}
