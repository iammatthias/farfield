package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"  // register the GIF decoder
	_ "image/jpeg" // register the JPEG decoder
	_ "image/png"  // register the PNG decoder
	"net/http"

	"github.com/buckket/go-blurhash"
	_ "golang.org/x/image/webp" // register the WebP decoder
)

// Meta is the metadata farfield keeps for one blob. It is the SQLite row, the
// JSON API response, and — during migration — the shape of the R2 `.json`
// sidecars being imported. The bytes themselves live in the ByteStore.
type Meta struct {
	CID           string `json:"cid"`
	Size          int64  `json:"size"`
	Mime          string `json:"mime"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	Blurhash      string `json:"blurhash,omitempty"`
	DominantColor string `json:"dominantColor,omitempty"`
	CreatedAt     string `json:"createdAt,omitempty"`
}

// DeriveMetadata hashes data and derives its metadata. Decodable images get
// dimensions, a blurhash, and a dominant color; other media are stored
// opaquely with just a size and a sniffed MIME type.
func DeriveMetadata(data []byte) (Meta, error) {
	m := Meta{CID: BlobCID(data), Size: int64(len(data))}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// Not a decodable image — store it opaquely.
		m.Mime = http.DetectContentType(data)
		return m, nil
	}
	bh, err := blurhash.Encode(4, 3, img)
	if err != nil {
		return Meta{}, fmt.Errorf("blurhash: %w", err)
	}
	b := img.Bounds()
	m.Mime = mimeFor(format)
	m.Width = b.Dx()
	m.Height = b.Dy()
	m.Blurhash = bh
	m.DominantColor = averageColor(img)
	return m, nil
}

func mimeFor(format string) string {
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// averageColor returns the mean color of an image as a hex string. It works
// in straight (non-premultiplied) color so a semi-transparent pixel
// contributes its actual hue, not an alpha-darkened one.
func averageColor(img image.Image) string {
	b := img.Bounds()
	var r, g, bl, n uint64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			r += uint64(c.R)
			g += uint64(c.G)
			bl += uint64(c.B)
			n++
		}
	}
	if n == 0 {
		return "#000000"
	}
	return fmt.Sprintf("#%02x%02x%02x", r/n, g/n, bl/n)
}
