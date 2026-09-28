package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"strings"

	"github.com/gen2brain/heic"
)

// Everything uploaded here is served publicly and cached at the edge for a
// year, and blobs are content-addressed, so a photo's metadata becomes part
// of its permanent public identity the moment it is stored. A phone photo
// carries where it was taken: GPS coordinates sit in its EXIF (and often
// again in XMP), and publishing them means publishing where someone lives.
//
// So images are sanitized BEFORE they are hashed. The CID is the CID of the
// clean bytes, a re-upload of the same original dedupes to the same clean
// blob, and nothing downstream ever sees the tagged version.
//
// What changes, by format:
//
//   - HEIC/HEIF (what an iPhone shoots) becomes a JPEG. Only Safari can show
//     HEIC; everywhere else an iPhone photo was a broken image, with no
//     dimensions, blurhash, or thumbnail. libheif applies the image's rotation
//     on decode and none of its metadata survives the re-encode.
//   - JPEG loses its EXIF/XMP (APP1) and IPTC (APP13) segments; the ICC
//     profile (APP2) and everything else is kept byte for byte. Dropping EXIF
//     drops the Orientation tag browsers use to turn a sideways-stored photo
//     upright, so a rotated photo has its pixels turned first (one re-encode);
//     an upright one is stripped losslessly.
//   - PNG loses its eXIf chunk and its text chunks (tEXt/zTXt/iTXt), which is
//     where EXIF and XMP ride in a PNG.
//
// Anything else passes through untouched.

// heicJPEGQuality is high because the JPEG IS the photo now, not a preview.
const heicJPEGQuality = 90

// rotatedJPEGQuality is the re-encode quality when a JPEG must be turned.
const rotatedJPEGQuality = 92

// sanitizeImage returns data with location and other metadata removed, and
// HEIC converted to JPEG. It never fails an upload: bytes it cannot parse
// are returned as they came, since refusing a photo is worse than storing
// one this code did not understand.
func sanitizeImage(data []byte) []byte {
	switch mime := sniffMime(data); {
	case mime == "image/heic" || mime == "image/heif":
		if out, ok := heicToJPEG(data); ok {
			return out
		}
	case strings.HasPrefix(mime, "image/jpeg"):
		if out, ok := stripJPEG(data); ok {
			return out
		}
	case mime == "image/png":
		if out, ok := stripPNG(data); ok {
			return out
		}
	}
	return data
}

func heicToJPEG(data []byte) ([]byte, bool) {
	cfg, err := heic.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width > maxDecodeDim || cfg.Height > maxDecodeDim {
		return nil, false
	}
	img, err := heic.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: heicJPEGQuality}); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// ── JPEG ───────────────────────────────────────────────────────────────────

// stripJPEG removes APP1 and APP13 segments. It walks the marker segments up
// to the start of scan and copies the rest verbatim, so the compressed image
// data is never touched.
func stripJPEG(data []byte) ([]byte, bool) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, false
	}
	orientation := 1
	var out bytes.Buffer
	out.Write(data[:2]) // SOI
	i := 2
	sawScan := false
	for i+4 <= len(data) {
		if data[i] != 0xFF {
			return nil, false
		}
		marker := data[i+1]
		if marker == 0xFF { // fill byte
			i++
			continue
		}
		if marker == 0xDA { // start of scan: the rest is image data
			out.Write(data[i:])
			sawScan = true
			break
		}
		if marker == 0xD9 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			out.Write(data[i : i+2]) // markers without a length
			i += 2
			continue
		}
		n := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if n < 2 || i+2+n > len(data) {
			return nil, false
		}
		seg := data[i : i+2+n]
		switch marker {
		case 0xE1: // APP1: EXIF or XMP
			if o := exifOrientation(seg[4:]); o != 0 {
				orientation = o
			}
			// dropped
		case 0xED: // APP13: IPTC / Photoshop resources
			// dropped
		default:
			out.Write(seg)
		}
		i += 2 + n
	}
	if !sawScan {
		return nil, false // no image data found: not a JPEG this code understands
	}
	if orientation == 1 {
		return out.Bytes(), true
	}
	// The pixels are stored turned; without the tag a browser would show them
	// sideways. Decode, turn upright, re-encode.
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width > maxDecodeDim || cfg.Height > maxDecodeDim {
		return out.Bytes(), true
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return out.Bytes(), true
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, orient(img, orientation), &jpeg.Options{Quality: rotatedJPEGQuality}); err != nil {
		return out.Bytes(), true
	}
	return buf.Bytes(), true
}

// exifOrientation reads the Orientation tag (0x0112) from an APP1 payload,
// returning 0 when the segment is not EXIF or carries no valid orientation.
func exifOrientation(p []byte) int {
	if len(p) < 14 || string(p[:6]) != "Exif\x00\x00" {
		return 0
	}
	t := p[6:] // TIFF header
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	ifd := int(bo.Uint32(t[4:8]))
	if ifd+2 > len(t) {
		return 0
	}
	count := int(bo.Uint16(t[ifd : ifd+2]))
	for e := 0; e < count; e++ {
		off := ifd + 2 + e*12
		if off+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[off:off+2]) == 0x0112 {
			if v := int(bo.Uint16(t[off+8 : off+10])); v >= 1 && v <= 8 {
				return v
			}
			return 0
		}
	}
	return 0
}

// orient applies an EXIF orientation (2–8), returning an upright image.
// Each case maps a destination pixel back to its source pixel.
func orient(src image.Image, o int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 { // 5–8 swap the axes
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch o {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			default:
				sx, sy = x, y
			}
			dst.Set(x, y, src.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}

// ── PNG ────────────────────────────────────────────────────────────────────

var pngSig = []byte("\x89PNG\r\n\x1a\n")

// stripPNG drops the eXIf and text chunks, copying every other chunk as-is.
func stripPNG(data []byte) ([]byte, bool) {
	if !bytes.HasPrefix(data, pngSig) {
		return nil, false
	}
	var out bytes.Buffer
	out.Write(pngSig)
	i := len(pngSig)
	sawEnd := false
	for i+12 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[i : i+4]))
		end := i + 12 + n
		if n < 0 || end > len(data) {
			return nil, false
		}
		typ := string(data[i+4 : i+8])
		// Verify the chunk before trusting its framing.
		if crc32.ChecksumIEEE(data[i+4:i+8+n]) != binary.BigEndian.Uint32(data[i+8+n:end]) {
			return nil, false
		}
		switch typ {
		case "eXIf", "tEXt", "zTXt", "iTXt":
			// dropped
		default:
			out.Write(data[i:end])
		}
		i = end
		if typ == "IEND" {
			sawEnd = true
			break
		}
	}
	// A file that never reaches IEND is truncated or not a PNG at all:
	// leave it exactly as uploaded rather than store a shortened copy.
	if !sawEnd {
		return nil, false
	}
	return out.Bytes(), true
}
