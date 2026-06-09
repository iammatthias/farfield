package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // register the GIF decoder
	"image/jpeg"  // decoder + thumbnail encoder
	_ "image/png" // register the PNG decoder
	"net/http"

	"github.com/buckket/go-blurhash"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register the WebP decoder
)

const (
	// thumbMaxDim is the longest side of a generated thumbnail.
	thumbMaxDim = 320
	// maxDecodeDim caps the dimensions this service will decode. The check
	// runs on the image header (image.DecodeConfig) before any pixel work,
	// so a decompression bomb never allocates — 12000×12000 RGBA alone
	// would be ~550 MB.
	maxDecodeDim = 12000
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
	ThumbCID      string `json:"thumbCid,omitempty"`
	CreatedAt     string `json:"createdAt,omitempty"`
}

// DeriveMetadata hashes data and derives its metadata. Decodable images get
// dimensions, a blurhash, and a dominant color; other media are stored
// opaquely with just a size and a sniffed MIME type.
func DeriveMetadata(data []byte) (Meta, error) {
	m, _, err := deriveMetadata(data)
	return m, err
}

// deriveMetadata is DeriveMetadata plus the decoded image (nil for non-images
// and for images skipped by the size sanity check), so the upload path can
// generate a thumbnail without paying for a second decode.
func deriveMetadata(data []byte) (Meta, image.Image, error) {
	m := Meta{CID: BlobCID(data), Size: int64(len(data))}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width > maxDecodeDim || cfg.Height > maxDecodeDim {
		// Not a decodable image — or one with absurd dimensions, which is
		// never worth a full pixel decode — store it opaquely.
		m.Mime = http.DetectContentType(data)
		return m, nil, nil
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// Header parsed but the pixels did not (truncated file) — opaque.
		m.Mime = http.DetectContentType(data)
		return m, nil, nil
	}
	bh, err := blurhash.Encode(4, 3, img)
	if err != nil {
		return Meta{}, nil, fmt.Errorf("blurhash: %w", err)
	}
	b := img.Bounds()
	m.Mime = mimeFor(format)
	m.Width = b.Dx()
	m.Height = b.Dy()
	m.Blurhash = bh
	m.DominantColor = averageColor(img)
	return m, img, nil
}

// thumbJPEG downscales img so its longest side is thumbMaxDim and encodes it
// as JPEG (quality 80). It returns nil when no thumbnail applies — the image
// already fits within thumbMaxDim — or when encoding fails; callers fall back
// to serving the full blob.
func thumbJPEG(img image.Image) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || (w <= thumbMaxDim && h <= thumbMaxDim) {
		return nil
	}
	tw, th := thumbMaxDim, thumbMaxDim
	if w >= h {
		th = max(1, h*thumbMaxDim/w)
	} else {
		tw = max(1, w*thumbMaxDim/h)
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, xdraw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil
	}
	return buf.Bytes()
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
// contributes its actual hue, not an alpha-darkened one. It samples every
// 8th pixel on both axes — ~64× fewer conversions, visually identical.
func averageColor(img image.Image) string {
	const step = 8
	b := img.Bounds()
	var r, g, bl, n uint64
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
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
