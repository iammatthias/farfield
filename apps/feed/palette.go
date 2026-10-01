package main

// feed's part of the ⌘K menu: a new post to start, and the recent posts
// there are to open.

import (
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	items := []web.PaletteItem{
		{Kind: "action", Title: "New post", URL: "/new", Words: "write compose note"},
	}
	if posts, err := listPosts(s.db, web.PaletteLimit, ""); err == nil {
		for _, p := range posts {
			items = append(items, web.PaletteItem{Kind: "record", Title: postTitle(p), Sub: paletteDate(p.CreatedAt),
				URL: "/posts/" + p.Slug + "/edit", Words: strings.TrimSpace(p.Slug + " " + strings.Join(p.Tags, " "))})
		}
	}
	return items
}

// a title reads as text: links keep their words, emphasis loses its marks
var (
	mdLink  = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdMarks = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "", "*", "")
)

// postTitle names a post, which has no title of its own, by its first line of
// text — Markdown markers stripped, cut short — or its slug when it is empty.
func postTitle(p Post) string {
	for _, line := range strings.Split(p.Body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#>*-+ "))
		line = mdLink.ReplaceAllString(line, "$1")
		line = strings.TrimSpace(mdMarks.Replace(line))
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > 80 {
			line = string([]rune(line)[:80]) + "…"
		}
		return line
	}
	return p.Slug
}

// paletteDate is the day part of an RFC 3339 timestamp.
func paletteDate(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}
