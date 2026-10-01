package main

// keys' part of the ⌘K menu: the key to issue, and the keys there are —
// by name, app and scope only; a key has no page of its own, so each opens the index.

import (
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	items := []web.PaletteItem{
		{Kind: "action", Title: "New key", URL: "/new", Words: "issue mint token ffk"},
	}
	if ks, err := s.ks.List(); err == nil {
		for i, k := range ks {
			if i == web.PaletteLimit {
				break
			}
			v := keyView(k)
			items = append(items, web.PaletteItem{Kind: "record", Title: k.Name,
				Sub: k.App + " · " + k.Scope + " · " + strings.ToLower(v.Status),
				URL: "/", Words: k.App + " " + k.Scope})
		}
	}
	return items
}
