package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// ufoPageURL is the Department of War's public UFO imagery index. The scraper
// reads the server-rendered HTML only — it never executes page JavaScript.
const ufoPageURL = "https://www.war.gov/UFO/"

// ufoManifestURL is a public, static index of the same war.gov/ufo release. The
// official Akamai edge blocks some server-side fetches, so the scraper first
// tries war.gov HTML and then falls back to this manifest while preserving the
// original war.gov/DVIDS media URLs.
const ufoManifestURL = "https://raw.githubusercontent.com/vng9trmgr8-pixel/war-gov-ufo-release-1/refs/heads/main/data.json"

// ufoOrigin is the scheme+host used to resolve relative links found on the page.
const ufoOrigin = "https://www.war.gov"

// ufoAssetsOrigin is the image CDN used by uapufo.org. Its raw/image filenames
// mirror the war.gov release thumbnail filenames, and unlike war.gov's Akamai
// edge it is fetchable by normal browsers and server-side clients.
const ufoAssetsOrigin = "https://assets.uapufo.org"

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

// imageExts and videoExts classify a media URL by its file extension.
var (
	imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true,
		".gif": true, ".webp": true, ".avif": true, ".bmp": true}
	videoExts = map[string]bool{".mp4": true, ".mov": true, ".webm": true,
		".m4v": true, ".ogv": true}
)

// ufoScrape fetches the war.gov UFO page and parses its media entries. A
// non-2xx response, a transport error, or a page with no recognisable media
// all return an error — the caller then falls back to placeholder items so
// the app never fails because an upstream changed shape.
func (f *fetcher) ufoScrape() ([]Photo, error) {
	photos, err := f.ufoScrapeHTML()
	if err == nil && len(photos) > 0 {
		return photos, nil
	}
	manifestPhotos, manifestErr := f.ufoScrapeManifest()
	if manifestErr == nil && len(manifestPhotos) > 0 {
		return manifestPhotos, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, manifestErr
}

func (f *fetcher) ufoScrapeHTML() ([]Photo, error) {
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

type ufoManifest struct {
	Images []ufoManifestRecord `json:"images"`
	Videos []ufoManifestRecord `json:"videos"`
}

type ufoManifestRecord struct {
	Title      string `json:"title"`
	Blurb      string `json:"blurb"`
	URL        string `json:"url"`
	Thumb      string `json:"thumb"`
	VideoID    string `json:"videoId"`
	VideoTitle string `json:"videoTitle"`
	Agency     string `json:"agency"`
}

func (f *fetcher) ufoScrapeManifest() ([]Photo, error) {
	req, err := http.NewRequest(http.MethodGet, ufoManifestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("UFO manifest HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var manifest ufoManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("decoding UFO manifest: %w", err)
	}
	photos := photosFromUFOManifest(manifest)
	if len(photos) == 0 {
		return nil, fmt.Errorf("UFO manifest yielded no media")
	}
	return photos, nil
}

func photosFromUFOManifest(manifest ufoManifest) []Photo {
	photos := make([]Photo, 0, len(manifest.Images)+len(manifest.Videos))
	seen := map[string]bool{}
	add := func(p Photo) {
		key := p.ImageURL
		if key == "" {
			key = p.ThumbURL
		}
		if key == "" || seen[key] || len(photos) >= ufoMaxItems {
			return
		}
		seen[key] = true
		photos = append(photos, p)
	}
	for _, r := range manifest.Images {
		officialImage := strings.TrimSpace(r.URL)
		assetName := ufoAssetName(r.Thumb)
		if assetName == "" {
			assetName = ufoAssetName(r.URL)
		}
		if mediaKind(officialImage) != "image" && assetName == "" {
			continue
		}
		image, thumb := officialImage, strings.TrimSpace(r.Thumb)
		if assetName != "" {
			image = ufoAssetImageURL(assetName, 1600)
			thumb = ufoAssetImageURL(assetName, 640)
		}
		if thumb == "" {
			thumb = image
		}
		add(ufoManifestPhoto(r, image, thumb, "image", officialImage))
	}
	for _, r := range manifest.Videos {
		thumb := strings.TrimSpace(r.Thumb)
		if thumb == "" || mediaKind(thumb) != "image" {
			continue
		}
		video := ufoVideoURL(r.VideoID)
		if video == "" {
			video = strings.TrimSpace(r.URL)
		}
		add(ufoManifestPhoto(r, video, thumb, "video", video))
	}
	return photos
}

func ufoManifestPhoto(r ufoManifestRecord, image, thumb, media, sourceURL string) Photo {
	title := strings.TrimSpace(r.Title)
	if title == "" {
		title = strings.TrimSpace(r.VideoTitle)
	}
	if title == "" {
		title = titleFromURL(image)
	}
	credit := "U.S. Department of War"
	if agency := strings.TrimSpace(r.Agency); agency != "" {
		credit = agency + " · " + credit
	}
	if sourceURL == "" {
		sourceURL = ufoPageURL
	}
	return Photo{
		Source:      sourceUFO,
		Title:       title,
		Explanation: strings.TrimSpace(r.Blurb),
		ImageURL:    image,
		ThumbURL:    thumb,
		MediaType:   media,
		Credit:      credit,
		SourceURL:   sourceURL,
	}
}

func ufoVideoURL(videoID string) string {
	videoID = strings.TrimSpace(videoID)
	if videoID == "" {
		return ""
	}
	return "https://www.dvidshub.net/video/" + videoID
}

func ufoAssetName(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	name := path.Base(strings.Split(strings.Split(rawURL, "?")[0], "#")[0])
	if name == "." || name == "/" || name == "" {
		return ""
	}
	if strings.EqualFold(path.Ext(name), ".png") {
		name = strings.TrimSuffix(name, path.Ext(name)) + ".jpg"
	}
	return name
}

func ufoAssetImageURL(name string, width int) string {
	return fmt.Sprintf("%s/cdn-cgi/image/width=%d,quality=82,format=auto/raw/image/%s",
		ufoAssetsOrigin, width, name)
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

// mediaKind classifies a URL as "image" or "video" by extension, or "" when it
// is not a recognised media file.
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
