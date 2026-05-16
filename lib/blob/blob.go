// Package blob is the content-addressed blob store.
//
// A media file — image, video, PDF, anything — is bytes plus derived
// metadata, keyed by a content identifier. Images additionally carry
// dimensions, a blurhash, and a dominant color; other types carry just their
// size and MIME type. Store is the storage seam: LocalDir writes to a
// directory (local dev), R2 to a bucket (the server).
//
// Blobs are self-verifying: re-hashing the bytes reproduces the CID.
package blob

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"  // register GIF decoder
	_ "image/jpeg" // register JPEG decoder
	_ "image/png"  // register PNG decoder
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/buckket/go-blurhash"
	"github.com/iammatthias/farfield/lib/core"
	_ "golang.org/x/image/webp" // register WebP decoder
)

// Meta is everything known about one blob — returned by an upload and stored
// alongside the bytes. The image-only fields are omitted for other media.
type Meta struct {
	CID           string `json:"cid"`
	Size          int64  `json:"size"`
	Mime          string `json:"mime"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	Blurhash      string `json:"blurhash,omitempty"`
	DominantColor string `json:"dominantColor,omitempty"`
}

// DeriveMetadata hashes data and derives its metadata. If data decodes as an
// image it gets dimensions, a blurhash, and a dominant color; any other media
// type — video, PDF, … — is stored opaquely with just its size and a sniffed
// MIME type.
func DeriveMetadata(data []byte) (Meta, error) {
	meta := Meta{CID: core.BlobCID(data), Size: int64(len(data))}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// Not a decodable image. http.DetectContentType always returns a
		// type, falling back to application/octet-stream.
		meta.Mime = http.DetectContentType(data)
		return meta, nil
	}
	bh, err := blurhash.Encode(4, 3, img)
	if err != nil {
		return Meta{}, fmt.Errorf("blurhash: %w", err)
	}
	bounds := img.Bounds()
	meta.Mime = mimeFor(format)
	meta.Width = bounds.Dx()
	meta.Height = bounds.Dy()
	meta.Blurhash = bh
	meta.DominantColor = averageColor(img)
	return meta, nil
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

func averageColor(img image.Image) string {
	b := img.Bounds()
	var r, g, bl, n uint64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			// NRGBA = straight (non-premultiplied) color — the dominant
			// color of a semi-transparent pixel is its actual hue, not its
			// alpha-darkened one.
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

// Store is a content-addressed blob store, keyed by CID.
type Store interface {
	Put(meta Meta, data []byte) error
	GetBytes(cid string) ([]byte, error) // (nil, nil) if absent
	GetMeta(cid string) (*Meta, error)   // (nil, nil) if absent
	Exists(cid string) bool
	Delete(cid string) error
	List() ([]string, error)
}

// LocalDir is a blob store backed by a directory — `<cid>` holds the bytes,
// `<cid>.json` the metadata. The directory itself is the index.
type LocalDir struct {
	root string
}

// OpenLocalDir opens (creating if absent) a blob directory at root.
func OpenLocalDir(root string) (*LocalDir, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &LocalDir{root: root}, nil
}

func (d *LocalDir) bytesPath(cid string) string { return filepath.Join(d.root, cid) }
func (d *LocalDir) metaPath(cid string) string  { return filepath.Join(d.root, cid+".json") }

// Put stores a blob's bytes and metadata.
func (d *LocalDir) Put(meta Meta, data []byte) error {
	if err := os.WriteFile(d.bytesPath(meta.CID), data, 0o644); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(d.metaPath(meta.CID), encoded, 0o644)
}

// GetBytes fetches a blob's bytes, or (nil, nil) if absent.
func (d *LocalDir) GetBytes(cid string) ([]byte, error) {
	data, err := os.ReadFile(d.bytesPath(cid))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

// GetMeta fetches a blob's metadata, or (nil, nil) if absent.
func (d *LocalDir) GetMeta(cid string) (*Meta, error) {
	data, err := os.ReadFile(d.metaPath(cid))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Exists reports whether a blob is stored.
func (d *LocalDir) Exists(cid string) bool {
	_, err := os.Stat(d.bytesPath(cid))
	return err == nil
}

// Delete removes a blob's bytes and metadata.
func (d *LocalDir) Delete(cid string) error {
	for _, p := range []string{d.bytesPath(cid), d.metaPath(cid)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// List returns every stored blob CID.
func (d *LocalDir) List() ([]string, error) {
	entries, err := os.ReadDir(d.root)
	if err != nil {
		return nil, err
	}
	var cids []string
	for _, e := range entries {
		name := e.Name()
		// The bytes file is named by the bare CID; skip the .json sidecars.
		if !strings.HasSuffix(name, ".json") {
			cids = append(cids, name)
		}
	}
	sort.Strings(cids)
	return cids, nil
}
