package main

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/color"
	"image/png"
)

// Icons in a release .ipa are Apple-crushed (CgBI) PNGs baked into the asset
// catalog — not cleanly decodable, and only cosmetic in the install prompt (the
// real icon appears once installed). So sideload generates its own clean,
// deterministic 5×5 symmetric identicon per app: a stable ink mark on farfield
// paper that distinguishes apps in the prompt without fighting the bundle.

var (
	iconPaper = color.RGBA{0xfa, 0xf9, 0xf6, 0xff} // farfield surface
	iconInk   = color.RGBA{0x1a, 0x1a, 0x1a, 0xff} // farfield foreground
)

// iconPNG renders the identicon for seed at the given square pixel size.
func iconPNG(seed string, size int) ([]byte, error) {
	if size < 5 {
		size = 5
	}
	h := sha256.Sum256([]byte(seed))

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	fill(img, image.Rect(0, 0, size, size), iconPaper)

	// A one-cell margin around a 5×5 grid keeps the mark off the mask edge.
	const grid = 5
	cell := size / (grid + 2)
	if cell < 1 {
		cell = 1
	}
	off := (size - cell*grid) / 2

	// 15 cells (cols 0..2 × 5 rows) decide the left half; cols 3,4 mirror.
	for col := 0; col < 3; col++ {
		for row := 0; row < grid; row++ {
			if h[col*grid+row]&1 == 0 {
				continue
			}
			drawCell(img, off, cell, col, row)
			if mirror := grid - 1 - col; mirror != col {
				drawCell(img, off, cell, mirror, row)
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func drawCell(img *image.RGBA, off, cell, col, row int) {
	x0 := off + col*cell
	y0 := off + row*cell
	fill(img, image.Rect(x0, y0, x0+cell, y0+cell), iconInk)
}

func fill(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}
