package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"testing"
)

// splitImage is 40x20: left half red, right half blue. After any rotation
// the colors say which way is up.
func splitImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			c := color.RGBA{220, 30, 30, 255}
			if x >= 20 {
				c = color.RGBA{30, 30, 220, 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// exifAPP1 builds an EXIF APP1 segment carrying an Orientation tag and a
// GPSInfo IFD with a latitude — the shape a phone writes.
func exifAPP1(orientation uint16) []byte {
	bo := binary.BigEndian
	var t bytes.Buffer
	t.WriteString("MM")
	binary.Write(&t, bo, uint16(42))
	binary.Write(&t, bo, uint32(8)) // IFD0 at 8
	// IFD0: 2 entries — Orientation, GPSInfo pointer.
	binary.Write(&t, bo, uint16(2))
	gpsOff := uint32(8 + 2 + 2*12 + 4)
	for _, e := range [][4]uint32{{0x0112, 3, 1, uint32(orientation) << 16}, {0x8825, 4, 1, gpsOff}} {
		binary.Write(&t, bo, uint16(e[0]))
		binary.Write(&t, bo, uint16(e[1]))
		binary.Write(&t, bo, e[2])
		binary.Write(&t, bo, e[3])
	}
	binary.Write(&t, bo, uint32(0)) // no next IFD
	// GPS IFD: GPSLatitudeRef = "N".
	binary.Write(&t, bo, uint16(1))
	binary.Write(&t, bo, uint16(0x0001))
	binary.Write(&t, bo, uint16(2))
	binary.Write(&t, bo, uint32(2))
	t.Write([]byte{'N', 0, 0, 0})
	binary.Write(&t, bo, uint32(0))

	payload := append([]byte("Exif\x00\x00"), t.Bytes()...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	return append(seg, payload...)
}

// taggedJPEG encodes img and splices an EXIF segment in after SOI.
func taggedJPEG(t *testing.T, img image.Image, orientation uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	out := append([]byte{}, b[:2]...)
	out = append(out, exifAPP1(orientation)...)
	return append(out, b[2:]...)
}

func assertNoExif(t *testing.T, b []byte) {
	t.Helper()
	if bytes.Contains(b, []byte("Exif\x00\x00")) {
		t.Error("EXIF survived sanitizing")
	}
}

func near(c color.Color, want color.RGBA) bool {
	r, g, b, _ := c.RGBA()
	d := func(a uint32, w uint8) bool { x := int(a>>8) - int(w); return x > -40 && x < 40 }
	return d(r, want.R) && d(g, want.G) && d(b, want.B)
}

var red, blue = color.RGBA{220, 30, 30, 255}, color.RGBA{30, 30, 220, 255}

func TestSanitizeStripsGPSFromUprightJPEGLosslessly(t *testing.T) {
	in := taggedJPEG(t, splitImage(), 1)
	out := sanitizeImage(in)
	assertNoExif(t, out)
	// Upright: only the segment was removed; the scan data is untouched.
	if len(in)-len(out) != len(exifAPP1(1)) {
		t.Errorf("size changed by %d, want exactly the EXIF segment (%d)", len(in)-len(out), len(exifAPP1(1)))
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("stripped JPEG does not decode: %v", err)
	}
}

func TestSanitizeTurnsARotatedJPEGUpright(t *testing.T) {
	// Orientation 6: stored 40x20, displayed rotated 90° clockwise → 20x40,
	// with the red (left) half on top.
	out := sanitizeImage(taggedJPEG(t, splitImage(), 6))
	assertNoExif(t, out)
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 20 || b.Dy() != 40 {
		t.Fatalf("bounds = %v, want 20x40", b)
	}
	if !near(img.At(10, 5), red) || !near(img.At(10, 35), blue) {
		t.Errorf("not upright: top=%v bottom=%v, want red over blue", img.At(10, 5), img.At(10, 35))
	}
}

func TestOrientAllEight(t *testing.T) {
	// Where the source's top-left red pixel region must land for each tag.
	src := splitImage()
	for o, wantRedTop := range map[int]bool{1: false, 2: false, 3: false, 4: false, 5: true, 6: true, 7: false, 8: false} {
		img := orient(src, o)
		b := img.Bounds()
		if o >= 5 && (b.Dx() != 20 || b.Dy() != 40) {
			t.Errorf("o=%d bounds %v, want swapped", o, b)
			continue
		}
		if o >= 5 {
			top := near(img.At(10, 5), red)
			if top != wantRedTop {
				t.Errorf("o=%d red on top = %v, want %v", o, top, wantRedTop)
			}
		}
	}
	// Spot-check the mirrors: 2 flips left-right, so blue ends up on the left.
	if !near(orient(src, 2).At(5, 10), blue) {
		t.Error("o=2 did not mirror horizontally")
	}
	if !near(orient(src, 3).At(5, 10), blue) {
		t.Error("o=3 did not rotate 180")
	}
	if !near(orient(src, 8).At(10, 5), blue) {
		t.Error("o=8 did not rotate counter-clockwise")
	}
}

func TestSanitizeStripsPNGMetadata(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, splitImage()); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	chunk := func(typ string, data []byte) []byte {
		c := make([]byte, 8, 12+len(data))
		binary.BigEndian.PutUint32(c, uint32(len(data)))
		copy(c[4:], typ)
		c = append(c, data...)
		crc := make([]byte, 4)
		binary.BigEndian.PutUint32(crc, crc32.ChecksumIEEE(c[4:]))
		return append(c, crc...)
	}
	// Splice an eXIf and an XMP iTXt chunk in after IHDR (8 + 25 bytes).
	in := append([]byte{}, b[:33]...)
	in = append(in, chunk("eXIf", exifAPP1(1)[10:])...)
	in = append(in, chunk("iTXt", []byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00<gps/>"))...)
	in = append(in, b[33:]...)

	out := sanitizeImage(in)
	if bytes.Contains(out, []byte("eXIf")) || bytes.Contains(out, []byte("adobe.xmp")) {
		t.Error("PNG metadata survived")
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("stripped PNG does not decode: %v", err)
	}
	if !bytes.Equal(out, b) {
		t.Error("stripping did not restore the original, metadata-free PNG exactly")
	}
}

func TestSanitizeConvertsHEICToJPEG(t *testing.T) {
	in, err := os.ReadFile("testdata/synthetic.heic")
	if err != nil {
		t.Fatal(err)
	}
	out := sanitizeImage(in)
	if sniffMime(out) != "image/jpeg" {
		t.Fatalf("HEIC became %q, want image/jpeg", sniffMime(out))
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 40 || b.Dy() != 20 {
		t.Errorf("bounds = %v, want 40x20", b)
	}
	if !near(img.At(5, 10), red) || !near(img.At(35, 10), blue) {
		t.Errorf("colors wrong: left=%v right=%v", img.At(5, 10), img.At(35, 10))
	}
}

// An upload of a HEIC photo is stored as a JPEG with full metadata — the CID
// is the JPEG's, and re-uploading the same HEIC dedupes to it.
func TestUploadStoresHEICAsJPEG(t *testing.T) {
	s := uploadServer(t)
	in, err := os.ReadFile("testdata/synthetic.heic")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.storeUpload(in)
	if err != nil {
		t.Fatal(err)
	}
	if m.Mime != "image/jpeg" || m.Width != 40 || m.Height != 20 || m.Blurhash == "" {
		t.Errorf("meta = %+v, want a 40x20 JPEG with a blurhash", m)
	}
	again, err := s.storeUpload(in)
	if err != nil || again.CID != m.CID {
		t.Errorf("re-upload CID %v (%v), want %s", again, err, m.CID)
	}
}

// Bytes this code does not understand pass through untouched.
func TestSanitizeLeavesOtherBytesAlone(t *testing.T) {
	for _, in := range [][]byte{[]byte("plain text"), {0xFF, 0xD8, 0x00}, []byte("\x89PNG\r\n\x1a\ntruncated")} {
		if out := sanitizeImage(in); !bytes.Equal(out, in) {
			t.Errorf("%q changed", in)
		}
	}
}
