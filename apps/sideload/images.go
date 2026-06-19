package main

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"  // register decoders for DecodeConfig
	_ "image/jpeg" // (iOS screenshots are PNG; JPEG/GIF accepted too)
	_ "image/png"
	"net/http"
)

// maxScreenshotBytes caps a screenshot upload — generous for a full-resolution
// phone screenshot, far below the .ipa cap.
const maxScreenshotBytes = 12 << 20

// imageKinds maps an accepted content type to its stored file extension.
var imageKinds = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
}

// imageInfo validates that data is a supported image and returns its mime,
// extension, and pixel dimensions.
func imageInfo(data []byte) (mime, ext string, width, height int, err error) {
	mime = http.DetectContentType(data)
	ext, ok := imageKinds[mime]
	if !ok {
		return "", "", 0, 0, fmt.Errorf("unsupported image type %q — use PNG, JPEG, or GIF", mime)
	}
	cfg, _, derr := image.DecodeConfig(bytes.NewReader(data))
	if derr != nil {
		return "", "", 0, 0, fmt.Errorf("not a decodable image: %w", derr)
	}
	return mime, ext, cfg.Width, cfg.Height, nil
}
