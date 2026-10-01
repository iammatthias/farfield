package main

// scrap's part of the ⌘K menu: the paste to start, and the pastes there
// are to open. Only paste pages — never a magic link or token.

import (
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	items := []web.PaletteItem{
		{Kind: "action", Title: "New paste", URL: "/", Words: "compose scrap snippet"},
	}
	if pastes, err := s.palettePastes(); err == nil {
		for _, p := range pastes {
			title := p.Title
			if title == "" {
				title = p.ID
			}
			sub := p.Visibility
			if p.Lang != "" {
				sub += " · " + p.Lang
			}
			items = append(items, web.PaletteItem{Kind: "record", Title: title, Sub: sub,
				URL: "/" + p.ID, Words: strings.TrimSpace(p.ID + " " + p.Lang)})
		}
	}
	return items
}

// palettePastes is the newest unexpired pastes, without their bodies —
// listManagePastes reads every body and has no limit.
func (s *Server) palettePastes() ([]Paste, error) {
	rows, err := s.db.Query(`SELECT id, title, lang, visibility FROM pastes
		WHERE expires_at = '' OR expires_at > ?
		ORDER BY created_at DESC, id LIMIT ?`, store.NowRFC3339(), web.PaletteLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Paste{}
	for rows.Next() {
		var p Paste
		if err := rows.Scan(&p.ID, &p.Title, &p.Lang, &p.Visibility); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
