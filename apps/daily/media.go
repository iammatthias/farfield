package main

import (
	"path"
	"strings"
)

// imageExts and videoExts classify a media URL by file extension. NASA's APOD
// is an image or a video each day; the photo page renders each in kind.
var (
	imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true,
		".gif": true, ".webp": true, ".avif": true, ".bmp": true}
	videoExts = map[string]bool{".mp4": true, ".webm": true, ".mov": true,
		".m4v": true, ".ogv": true}
)

// mediaKind classifies a URL as "image" or "video" by extension, or "" when it
// is not a recognised media file. The photo template uses it to decide whether
// a video URL is a file it can play inline versus an embed it must link out to.
func mediaKind(u string) string {
	clean := u
	if i := strings.IndexAny(clean, "?#"); i >= 0 {
		clean = clean[:i]
	}
	switch ext := strings.ToLower(path.Ext(clean)); {
	case imageExts[ext]:
		return "image"
	case videoExts[ext]:
		return "video"
	default:
		return ""
	}
}
