package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// stampedSlug matches a slug that already carries a millisecond-epoch prefix —
// the "<unixMillis>-…" shape that keys both migrated and app-authored entries.
// Thirteen-plus digits is the exact form stampSlug produces; a shorter
// numeric run (a title like "100-days-of-code") is not mistaken for a stamp.
var stampedSlug = regexp.MustCompile(`^[0-9]{13,}-`)

// slugify turns arbitrary text into a URL-safe slug: lowercase, with runs of
// non-alphanumerics collapsed to single hyphens.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// stampSlug prefixes a slug with the millisecond epoch of t —
// "<unixMillis>-<slug>" — so app-authored entries are keyed exactly like the
// content we migrated: chronologically sortable, collision-free, and stable
// even if the title is later edited. An empty slug, or one that already
// carries such a prefix, is returned unchanged, keeping inserts idempotent.
func stampSlug(slug string, t time.Time) string {
	if slug == "" || stampedSlug.MatchString(slug) {
		return slug
	}
	return strconv.FormatInt(t.UnixMilli(), 10) + "-" + slug
}

// splitTags parses a comma-separated tag input into a trimmed, de-duplicated
// slice. Empty input yields an empty slice.
func splitTags(s string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
