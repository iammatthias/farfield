package main

import (
	"bytes"
	"image"
	"image/jpeg"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register the WebP decoder for cover images
	_ "image/gif"               // register the GIF decoder for cover images
	_ "image/png"               // register the PNG decoder for cover images
)

const (
	// thumbHeight is the target height of a cover thumbnail — list views and
	// OPDS thumbnail links render covers far smaller than the embedded image.
	thumbHeight = 300
	// thumbMaxPixels caps the source dimensions before a full decode — a
	// sanity check against decompression bombs hiding in an EPUB cover.
	thumbMaxPixels = 50 << 20 // ~52 MP
)

// makeThumb downscales cover image bytes to a thumbHeight-tall JPEG (q80).
// It returns nil when no thumbnail is warranted: the bytes do not decode as
// an image, the cover is already small, or its claimed dimensions are
// implausible. A nil result is not an error — the full cover serves instead.
func makeThumb(cover []byte) []byte {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(cover))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil
	}
	if cfg.Height <= thumbHeight {
		return nil // already thumbnail-sized — the original is the thumb
	}
	if cfg.Width*cfg.Height > thumbMaxPixels {
		return nil
	}

	src, _, err := image.Decode(bytes.NewReader(cover))
	if err != nil {
		return nil
	}
	w := cfg.Width * thumbHeight / cfg.Height
	if w < 1 {
		w = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, thumbHeight))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil
	}
	return buf.Bytes()
}
