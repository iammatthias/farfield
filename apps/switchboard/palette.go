package main

// switchboard's part of the ⌘K menu: nothing beyond its home. The console is
// one log page; its rows have no page of their own and name senders by phone.

import (
	"net/http"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	return nil
}
