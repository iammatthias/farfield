package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// userAgent identifies the bookmarks fetcher to the upstream site. A real
// product UA improves the chance of getting a real HTML response back rather
// than a bot-detection page.
const userAgent = "farfield-bookmarks/1.0 (+https://farfield.systems)"

// fetchTimeout caps a metadata fetch — including connect, TLS, and body read.
const fetchTimeout = 10 * time.Second

// maxFetchBytes caps the bytes we read from a target page. The interesting
// metadata lives in the first few KB of <head>; reading more is wasted I/O.
const maxFetchBytes = 128 * 1024

// metaResult is the bag of fields extracted from an HTML page.
type metaResult struct {
	Title         string
	Description   string
	Author        string
	OGTitle       string
	OGDescription string
	OGImage       string
	OGSiteName    string
	OGType        string
	Favicon       string
}

// fetchMetadata retrieves rawURL and returns the metadata extracted from its
// HTML. A network or parse failure returns an empty result and an error — the
// caller treats that as "do not block the save" and stores what it can.
func fetchMetadata(ctx context.Context, client *http.Client, rawURL string) (metaResult, error) {
	if strings.TrimSpace(rawURL) == "" {
		return metaResult{}, errors.New("empty url")
	}
	base, err := url.Parse(rawURL)
	if err != nil {
		return metaResult{}, fmt.Errorf("parsing url: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return metaResult{}, fmt.Errorf("unsupported scheme %q", base.Scheme)
	}

	reqCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return metaResult{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		return metaResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return metaResult{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Only HTML carries the metadata we scan for — bail before reading the
	// body when the server declares something else (a PDF, an image, a zip).
	// A missing Content-Type is given the benefit of the doubt.
	if ct := resp.Header.Get("Content-Type"); ct != "" &&
		!strings.Contains(strings.ToLower(ct), "html") {
		return metaResult{}, fmt.Errorf("not HTML: Content-Type %q", ct)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return metaResult{}, err
	}

	// resp.Request.URL is the post-redirect URL — that's the right base for
	// resolving relative og:image/favicon hrefs.
	finalBase := resp.Request.URL
	if finalBase == nil {
		finalBase = base
	}
	return extractMeta(body, finalBase), nil
}

// extractMeta scans HTML bytes for the head metadata farfield cares about.
// The scan is a hand-rolled tokenizer — stdlib has no HTML parser — but it
// handles the realistic shapes <meta>, <link>, and <title> appear in: quoted
// or unquoted attribute values, mixed case, and entity-escaped contents.
// Relative og:image and favicon URLs are resolved against base.
func extractMeta(body []byte, base *url.URL) metaResult {
	var m metaResult
	titleStart := -1
	titleEnd := -1

	src := body
	n := len(src)
	i := 0
scan:
	for i < n {
		lt := bytes.IndexByte(src[i:], '<')
		if lt < 0 {
			break
		}
		lt += i

		// comments and doctype-style declarations: skip to their close.
		if lt+1 < n && src[lt+1] == '!' {
			if lt+3 < n && src[lt+2] == '-' && src[lt+3] == '-' {
				end := bytes.Index(src[lt+4:], []byte("-->"))
				if end < 0 {
					break scan
				}
				i = lt + 4 + end + 3
				continue
			}
			gt := bytes.IndexByte(src[lt:], '>')
			if gt < 0 {
				break scan
			}
			i = lt + gt + 1
			continue
		}

		gt := bytes.IndexByte(src[lt:], '>')
		if gt < 0 {
			break scan
		}
		tag := src[lt+1 : lt+gt] // contents between '<' and '>'
		nextI := lt + gt + 1

		// Skip whitespace at the start.
		tag = bytes.TrimLeft(tag, " \t\r\n")
		if len(tag) == 0 {
			i = nextI
			continue
		}
		isClosing := false
		if tag[0] == '/' {
			isClosing = true
			tag = tag[1:]
		}
		// Parse the tag name.
		nameEnd := indexAnyByte(tag, " \t\r\n/")
		var name []byte
		var rest []byte
		if nameEnd < 0 {
			name = tag
		} else {
			name = tag[:nameEnd]
			rest = tag[nameEnd:]
		}
		lower := strings.ToLower(string(name))

		switch lower {
		case "title":
			if isClosing {
				if titleStart >= 0 && titleEnd < 0 {
					titleEnd = lt
				}
			} else if titleStart < 0 {
				titleStart = nextI
			}
		case "script", "style":
			if !isClosing {
				closeIdx := caseInsensitiveIndex(src[nextI:], []byte("</"+lower))
				if closeIdx < 0 {
					break scan
				}
				ngt := bytes.IndexByte(src[nextI+closeIdx:], '>')
				if ngt < 0 {
					break scan
				}
				nextI = nextI + closeIdx + ngt + 1
			}
		case "meta":
			applyMeta(&m, parseAttrs(rest), base)
		case "link":
			applyLink(&m, parseAttrs(rest), base)
		case "body":
			break scan
		}
		i = nextI
	}
	if titleStart >= 0 {
		if titleEnd < 0 {
			titleEnd = n
		}
		m.Title = strings.TrimSpace(html.UnescapeString(string(src[titleStart:titleEnd])))
	}
	if m.Favicon == "" {
		// Fall back to the well-known /favicon.ico for any host. This is the
		// browser's default lookup, so it is the right last-resort guess.
		if base != nil {
			fav := *base
			fav.Path = "/favicon.ico"
			fav.RawQuery = ""
			fav.Fragment = ""
			m.Favicon = fav.String()
		}
	}
	return m
}

// applyMeta interprets a <meta> tag's attributes and folds any recognised
// values into m. The first occurrence of each field wins so later, less
// canonical tags can't overwrite a head-of-head OpenGraph value.
func applyMeta(m *metaResult, a map[string]string, base *url.URL) {
	key := strings.ToLower(a["property"])
	if key == "" {
		key = strings.ToLower(a["name"])
	}
	content := strings.TrimSpace(a["content"])
	if key == "" || content == "" {
		return
	}
	switch key {
	case "description":
		if m.Description == "" {
			m.Description = content
		}
	case "author":
		if m.Author == "" {
			m.Author = content
		}
	case "og:title":
		if m.OGTitle == "" {
			m.OGTitle = content
		}
	case "og:description":
		if m.OGDescription == "" {
			m.OGDescription = content
		}
	case "og:image", "og:image:url", "og:image:secure_url":
		if m.OGImage == "" {
			m.OGImage = resolveAgainst(base, content)
		}
	case "og:site_name":
		if m.OGSiteName == "" {
			m.OGSiteName = content
		}
	case "og:type":
		if m.OGType == "" {
			m.OGType = content
		}
	}
}

// applyLink picks the favicon out of any <link rel="… icon …"> tag, resolving
// the href against the page's base URL.
func applyLink(m *metaResult, a map[string]string, base *url.URL) {
	rel := strings.ToLower(a["rel"])
	href := strings.TrimSpace(a["href"])
	if href == "" {
		return
	}
	if strings.Contains(rel, "icon") && m.Favicon == "" {
		m.Favicon = resolveAgainst(base, href)
	}
}

// parseAttrs reads attribute pairs from the body of a tag (everything after
// the tag name up to but not including '>'). It handles quoted, single-quoted,
// and unquoted values, and entity-escaped value contents.
func parseAttrs(s []byte) map[string]string {
	out := map[string]string{}
	i := 0
	for i < len(s) {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '/' {
			i++
			continue
		}
		start := i
		for i < len(s) && !isSpace(s[i]) && s[i] != '=' && s[i] != '/' {
			i++
		}
		name := strings.ToLower(string(s[start:i]))
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			if name != "" {
				out[name] = ""
			}
			continue
		}
		i++ // skip '='
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		var value string
		if s[i] == '"' || s[i] == '\'' {
			q := s[i]
			i++
			vstart := i
			for i < len(s) && s[i] != q {
				i++
			}
			value = string(s[vstart:i])
			if i < len(s) {
				i++
			}
		} else {
			vstart := i
			for i < len(s) && !isSpace(s[i]) && s[i] != '>' {
				i++
			}
			value = string(s[vstart:i])
		}
		if name != "" {
			out[name] = html.UnescapeString(value)
		}
	}
	return out
}

// resolveAgainst resolves ref against base. If ref is already absolute or
// base is nil, the input is returned unchanged.
func resolveAgainst(base *url.URL, ref string) string {
	if ref == "" || base == nil {
		return ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(u).String()
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f'
}

func indexAnyByte(s []byte, chars string) int {
	for i := 0; i < len(s); i++ {
		for j := 0; j < len(chars); j++ {
			if s[i] == chars[j] {
				return i
			}
		}
	}
	return -1
}

// caseInsensitiveIndex returns the index in s of the first occurrence of
// needle, comparing ASCII letters without regard to case. needle must be
// already lower-cased.
func caseInsensitiveIndex(s, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(s); i++ {
		ok := true
		for j := 0; j < len(needle); j++ {
			c := s[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// applyMetadata copies fetched metadata fields onto a bookmark. Existing
// non-empty fields on b are preserved — the admin can override the upstream
// title/description and a re-fetch should not clobber that.
func applyMetadata(b *Bookmark, m metaResult) {
	if b.Title == "" {
		// Prefer og:title over <title> when both are set — og:title is the
		// page's chosen social-share name and is usually cleaner.
		switch {
		case m.OGTitle != "":
			b.Title = m.OGTitle
		case m.Title != "":
			b.Title = m.Title
		}
	}
	if b.Description == "" {
		switch {
		case m.OGDescription != "":
			b.Description = m.OGDescription
		case m.Description != "":
			b.Description = m.Description
		}
	}
	// Fetched OG/meta fields always refresh on a refetch — that is the point of
	// the operation; the admin-provided title/description above are sticky.
	b.OGTitle = m.OGTitle
	b.OGDescription = m.OGDescription
	b.OGImage = m.OGImage
	b.OGSiteName = m.OGSiteName
	b.OGType = m.OGType
	b.MetaAuthor = m.Author
	b.Favicon = m.Favicon
}
