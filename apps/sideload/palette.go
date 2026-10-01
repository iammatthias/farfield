package main

// sideload's part of the ⌘K menu: the build to upload, and the apps and
// builds there are to open. Never a share or install link — those are tokens.

import (
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/web"
)

func (s *Server) paletteItems(r *http.Request) []web.PaletteItem {
	items := []web.PaletteItem{
		{Kind: "action", Title: "Upload a build", URL: "/#ipa", Words: "ipa new app version"},
	}
	if apps, err := listApps(s.db); err == nil {
		for i, a := range apps {
			if i == web.PaletteLimit {
				break
			}
			title := a.AppName
			if title == "" {
				title = a.BundleID
			}
			sub := "app"
			if a.Latest.Version != "" {
				sub += " · v" + a.Latest.Version
			}
			items = append(items, web.PaletteItem{Kind: "record", Title: title, Sub: sub,
				URL: "/app/" + a.BundleID, Words: a.BundleID})
		}
	}
	if builds, err := listBuilds(s.db); err == nil {
		for i, b := range builds {
			if i == web.PaletteLimit {
				break
			}
			title := b.AppName
			if title == "" {
				title = b.BundleID
			}
			if b.Version != "" {
				title += " v" + b.Version
			}
			if b.BuildNumber != "" {
				title += " (" + b.BuildNumber + ")"
			}
			sub := "build"
			if len(b.CreatedAt) >= 10 {
				sub += " · " + b.CreatedAt[:10]
			}
			items = append(items, web.PaletteItem{Kind: "record", Title: title, Sub: sub,
				URL: "/b/" + b.ID, Words: strings.TrimSpace(b.BundleID + " " + b.GitCommit + " " + b.ID)})
		}
	}
	return items
}
