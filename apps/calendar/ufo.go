package main

import (
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// ufoPageURL is the Department of War's public UFO imagery index. The scraper
// reads that page's server-rendered HTML only — it never executes page
// JavaScript and never ingests a third-party mirror of the release. If the
// page is unreachable or changes shape, the calendar falls back to placeholder
// entries (see ufoPlaceholders) rather than trusting an outside source.
const ufoPageURL = "https://www.war.gov/UFO/"

// ufoOrigin is the scheme+host used to resolve relative links found on the page.
const ufoOrigin = "https://www.war.gov"

// ufoMaxItems caps how many media items one scrape will keep — a conservative
// bound so a runaway page cannot flood the cache.
const ufoMaxItems = 365

// Attribute extractors. The page is server-rendered HTML; these pull the
// handful of attributes the scraper needs without a full HTML parser.
var (
	imgTagRe = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	aTagRe   = regexp.MustCompile(`(?is)<a\b[^>]*>`)
	srcAttr  = regexp.MustCompile(`(?is)\bsrc\s*=\s*["']([^"']+)["']`)
	hrefAttr = regexp.MustCompile(`(?is)\bhref\s*=\s*["']([^"']+)["']`)
	altAttr  = regexp.MustCompile(`(?is)\balt\s*=\s*["']([^"']*)["']`)
)

// imageExts, videoExts, and audioExts classify a media URL by file extension.
var (
	imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true,
		".gif": true, ".webp": true, ".avif": true, ".bmp": true}
	videoExts = map[string]bool{".mp4": true, ".mov": true, ".webm": true,
		".m4v": true, ".ogv": true}
	audioExts = map[string]bool{".mp3": true, ".wav": true, ".ogg": true,
		".oga": true, ".m4a": true, ".flac": true, ".aac": true}
)

// ufoScrape fetches the war.gov UFO page and parses its media entries. A
// non-2xx response, a transport error, or a page with no recognisable media
// all return an error — the caller then falls back to placeholder items so
// the app never fails because an upstream changed shape. The scraper reads
// only war.gov's own server-rendered HTML; there is no third-party fallback.
func (f *fetcher) ufoScrape() ([]Photo, error) {
	req, err := http.NewRequest(http.MethodGet, ufoPageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("war.gov UFO page HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	photos := parseUFOHTML(string(body))
	if len(photos) == 0 {
		return nil, fmt.Errorf("war.gov UFO page yielded no media")
	}
	return photos, nil
}

// parseUFOHTML extracts media entries from war.gov UFO page HTML. It is a pure
// function — no network — so it is directly testable. Dates are left blank;
// the cache layer assigns synthetic ordering dates at scrape time.
func parseUFOHTML(html string) []Photo {
	var photos []Photo
	seen := map[string]bool{}

	add := func(rawURL, alt string) {
		u := resolveURL(rawURL)
		if u == "" || seen[u] {
			return
		}
		media := mediaKind(u)
		if media == "" {
			return // not a recognised media file — skip
		}
		seen[u] = true
		title := strings.TrimSpace(alt)
		if title == "" {
			title = titleFromURL(u)
		}
		photos = append(photos, Photo{
			Source:    sourceUFO,
			Title:     title,
			ImageURL:  u,
			ThumbURL:  u,
			MediaType: media,
			Credit:    "U.S. Department of War",
			SourceURL: ufoPageURL,
			Explanation: "Declassified unidentified-anomalous-phenomena imagery " +
				"published by the U.S. Department of War at war.gov/UFO. " +
				"Mirrored and cached by farfield from the page's server-rendered HTML.",
		})
	}

	for _, tag := range imgTagRe.FindAllString(html, -1) {
		if m := srcAttr.FindStringSubmatch(tag); m != nil {
			alt := ""
			if a := altAttr.FindStringSubmatch(tag); a != nil {
				alt = a[1]
			}
			add(m[1], alt)
		}
	}
	for _, tag := range aTagRe.FindAllString(html, -1) {
		if m := hrefAttr.FindStringSubmatch(tag); m != nil {
			add(m[1], "")
		}
		if len(photos) >= ufoMaxItems {
			break
		}
	}
	if len(photos) > ufoMaxItems {
		photos = photos[:ufoMaxItems]
	}
	return photos
}

// resolveURL turns a possibly-relative HTML reference into an absolute URL,
// rejecting non-navigable references. It returns "" for anything unusable.
func resolveURL(ref string) string {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "", strings.HasPrefix(ref, "#"),
		strings.HasPrefix(ref, "data:"), strings.HasPrefix(ref, "javascript:"),
		strings.HasPrefix(ref, "mailto:"):
		return ""
	case strings.HasPrefix(ref, "//"):
		return "https:" + ref
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		return ref
	case strings.HasPrefix(ref, "/"):
		return ufoOrigin + ref
	default:
		return ufoOrigin + "/" + ref
	}
}

// mediaKind classifies a URL as "image", "video", or "audio" by extension, or
// "" when it is not a recognised media file.
func mediaKind(u string) string {
	clean := u
	if i := strings.IndexAny(clean, "?#"); i >= 0 {
		clean = clean[:i]
	}
	ext := strings.ToLower(path.Ext(clean))
	switch {
	case imageExts[ext]:
		return "image"
	case videoExts[ext]:
		return "video"
	case audioExts[ext]:
		return "audio"
	default:
		return ""
	}
}

// titleFromURL derives a readable title from a media file name.
func titleFromURL(u string) string {
	clean := u
	if i := strings.IndexAny(clean, "?#"); i >= 0 {
		clean = clean[:i]
	}
	name := path.Base(clean)
	name = strings.TrimSuffix(name, path.Ext(name))
	name = strings.NewReplacer("-", " ", "_", " ", "%20", " ").Replace(name)
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "UFO imagery"
	}
	return name
}

// ufoPlaceholders returns explanatory items used when the war.gov UFO page is
// unreachable or has changed shape. They keep the calendar usable instead of
// letting an upstream failure break the app.
func ufoPlaceholders() []Photo {
	explain := "The Department of War UFO imagery source could not be reached " +
		"or has changed format. This is a cached placeholder — the calendar " +
		"keeps working and will pick up real entries on the next successful " +
		"scrape of war.gov/UFO."
	items := []Photo{
		{Title: "UFO source — awaiting upstream"},
		{Title: "UFO source — declassified imagery"},
		{Title: "UFO source — cached placeholder"},
	}
	for i := range items {
		items[i].Source = sourceUFO
		items[i].Explanation = explain
		items[i].MediaType = "other"
		items[i].Credit = "U.S. Department of War"
		items[i].SourceURL = ufoPageURL
		items[i].Placeholder = true
	}
	return items
}
