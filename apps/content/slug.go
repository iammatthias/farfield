package main

import (
	"regexp"
	"strings"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify turns arbitrary text into a URL-safe slug: lowercase, with runs of
// non-alphanumerics collapsed to single hyphens.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
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
