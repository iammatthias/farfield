package capability

import (
	"net/url"
	"strings"
)

// ExtractTags pulls trailing hashtags off a body and returns the body without
// them.
//
// Hashtags are taken from the end and only from the end, because `#` at the start
// of a markdown line is a heading — treating every `#word` as a tag would quietly
// eat headings out of posts. Trailing is also how people actually write them:
// "walked the long way home #life".
func ExtractTags(body string) (string, []string) {
	fields := strings.Fields(body)
	var tags []string
	end := len(fields)
	for end > 0 {
		tag, ok := asHashtag(fields[end-1])
		if !ok {
			break
		}
		tags = append([]string{tag}, tags...)
		end--
	}
	if len(tags) == 0 {
		return strings.TrimSpace(body), nil
	}
	// Rebuild from the original text rather than re-joining fields, so internal
	// line breaks and spacing in the body survive.
	trimmed := body
	for i := len(fields) - 1; i >= end; i-- {
		if idx := strings.LastIndex(trimmed, fields[i]); idx >= 0 {
			trimmed = trimmed[:idx]
		}
	}
	return strings.TrimSpace(trimmed), Dedupe(tags)
}

// asHashtag validates one trailing token as a tag and returns it without the #.
func asHashtag(token string) (string, bool) {
	rest, ok := strings.CutPrefix(token, "#")
	if !ok || rest == "" {
		return "", false
	}
	for _, r := range rest {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return "", false
		}
	}
	return strings.ToLower(rest), true
}

// Dedupe drops empties and repeats, preserving order.
func Dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// SplitCommaList parses "a, b, c" into lowercase entries.
func SplitCommaList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// IsURL reports whether a string is a single http(s) URL and nothing else.
//
// It no longer decides routing — a bare link used to become a bookmark on its
// own, and now says nothing about which command was meant — but /bm still has to
// reject something that is plainly not a link before handing it to bookmarks.
func IsURL(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, " \t\n") {
		return false
	}
	u, err := url.Parse(text)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
