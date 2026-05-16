package blob

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
)

func testPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDeriveMetadata(t *testing.T) {
	png := testPNG(t, 8, 5, color.RGBA{200, 100, 50, 255})
	m, err := DeriveMetadata(png)
	if err != nil {
		t.Fatal(err)
	}
	if m.Width != 8 || m.Height != 5 {
		t.Fatalf("dimensions: %dx%d", m.Width, m.Height)
	}
	if m.Mime != "image/png" {
		t.Fatalf("mime: %s", m.Mime)
	}
	if !strings.HasPrefix(m.CID, "bafkrei") {
		t.Fatalf("expected a raw CIDv1, got %s", m.CID)
	}
	if !strings.HasPrefix(m.DominantColor, "#") || m.Blurhash == "" {
		t.Fatalf("metadata incomplete: %+v", m)
	}
	again, _ := DeriveMetadata(png)
	if again.CID != m.CID {
		t.Fatal("same bytes yielded different CIDs")
	}
}

func TestNonImageStoredOpaquely(t *testing.T) {
	// A PDF is not a decodable image — it should still get metadata: a CID, a
	// size, and a sniffed MIME type, but no image-only fields.
	m, err := DeriveMetadata([]byte("%PDF-1.4\n% non-image media\n"))
	if err != nil {
		t.Fatalf("non-image media should not error: %v", err)
	}
	if !strings.HasPrefix(m.CID, "bafkrei") || m.Size == 0 {
		t.Fatalf("expected a CID and size, got %+v", m)
	}
	if m.Mime == "" {
		t.Fatalf("expected a sniffed MIME type, got %+v", m)
	}
	if m.Width != 0 || m.Height != 0 || m.Blurhash != "" || m.DominantColor != "" {
		t.Fatalf("non-image should carry no image fields: %+v", m)
	}
}

func TestLocalDirRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blobs")
	store, err := OpenLocalDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := testPNG(t, 4, 4, color.RGBA{10, 20, 30, 255})
	meta, _ := DeriveMetadata(data)

	if err := store.Put(meta, data); err != nil {
		t.Fatal(err)
	}
	if !store.Exists(meta.CID) {
		t.Fatal("blob not found after Put")
	}
	got, _ := store.GetBytes(meta.CID)
	if !bytes.Equal(got, data) {
		t.Fatal("bytes round-trip mismatch")
	}
	gotMeta, _ := store.GetMeta(meta.CID)
	if gotMeta == nil || gotMeta.CID != meta.CID || gotMeta.Width != 4 {
		t.Fatalf("meta round-trip mismatch: %+v", gotMeta)
	}
	list, _ := store.List()
	if len(list) != 1 || list[0] != meta.CID {
		t.Fatalf("list: %v", list)
	}
	if err := store.Delete(meta.CID); err != nil {
		t.Fatal(err)
	}
	if store.Exists(meta.CID) {
		t.Fatal("blob survived Delete")
	}
}

func TestMissingBlobsReturnNil(t *testing.T) {
	store, _ := OpenLocalDir(filepath.Join(t.TempDir(), "empty"))
	if b, err := store.GetBytes("bafkmissing"); b != nil || err != nil {
		t.Fatalf("expected (nil, nil), got (%v, %v)", b, err)
	}
	if m, err := store.GetMeta("bafkmissing"); m != nil || err != nil {
		t.Fatalf("expected (nil, nil), got (%v, %v)", m, err)
	}
}
