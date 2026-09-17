package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The formatters the admin templates share.
//
// Each of these had been written separately in three to five apps, and the
// copies had drifted: humanSize stopped at MB in backup, blobs and library
// while sideload's equivalent went on to GB — so a 2 GB object read as
// "2048.0 MB" in the one app most likely to hold one. One implementation,
// and the largest unit wins everywhere.

// HumanSize renders a byte count for a person: "412 B", "8.4 KB", "1.2 GB".
func HumanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// RelAge renders an RFC3339 stamp as an age: "just now", "9m ago", "3d ago",
// and a plain date once it is older than two months — past which "63d ago"
// tells a reader less than the date does. An unparseable stamp is returned
// unchanged rather than swallowed, so a bad row is visible instead of blank.
func RelAge(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

// WantsJSON reports whether the client asked for JSON — how a handler decides
// between answering the editor's async save and redirecting a plain form post.
func WantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// FirstNonEmpty returns the first argument that is not empty, or "".
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
