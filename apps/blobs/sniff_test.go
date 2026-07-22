package main

import "testing"

// ftypHeader builds a minimal ISO-BMFF ftyp box: size, "ftyp", major brand,
// minor version, then any compatible brands.
func ftypHeader(major string, compat ...string) []byte {
	size := 16 + 4*len(compat)
	b := []byte{0, 0, 0, byte(size)}
	b = append(b, "ftyp"...)
	b = append(b, major...)
	b = append(b, 0, 0, 0, 0) // minor version
	for _, c := range compat {
		b = append(b, c...)
	}
	return b
}

func TestSniffMime(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		// Covered by Go's WHATWG sniffing already — must pass through.
		{"mp4", ftypHeader("mp42", "isom", "mp41"), "video/mp4"},
		{"webm", []byte{0x1A, 0x45, 0xDF, 0xA3, 0, 0, 0, 0, 0, 0, 0, 0}, "video/webm"},
		{"png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x00"), "image/png"},
		// The Apple brands Go misses — the refinement's whole point.
		{"quicktime mov", ftypHeader("qt  "), "video/quicktime"},
		{"m4v", ftypHeader("M4V "), "video/mp4"},
		{"m4a", ftypHeader("M4A "), "audio/mp4"},
		{"m4b audiobook", ftypHeader("M4B "), "audio/mp4"},
		// Unknowns stay opaque.
		{"junk", []byte{0xde, 0xad, 0xbe, 0xef, 0xde, 0xad, 0xbe, 0xef, 0, 0, 0, 0}, "application/octet-stream"},
		{"short", []byte{0x00}, "application/octet-stream"},
	}
	for _, c := range cases {
		if got := sniffMime(c.data); got != c.want {
			t.Errorf("%s: sniffMime = %q, want %q", c.name, got, c.want)
		}
	}
}
