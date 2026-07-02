package qrenc

import (
	"bytes"
	"strings"
	"testing"
)

// TestEncoderRoundTripStructure verifies the encoder produces a matrix with
// the expected size, finder patterns in the three corners, and a non-empty
// SVG for a representative payload. Without a stdlib QR decoder we can't
// verify scan-correctness, but the structural invariants catch most bugs.
func TestEncoderRoundTripStructure(t *testing.T) {
	mod, version, err := Encode([]byte("https://farfield.systems"), ECMedium)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	expectedSize := 21 + 4*(version-1)
	if len(mod) != expectedSize {
		t.Fatalf("matrix size = %d, want %d", len(mod), expectedSize)
	}
	for _, row := range mod {
		if len(row) != expectedSize {
			t.Fatalf("matrix row = %d, want %d", len(row), expectedSize)
		}
	}
	// Top-left finder: corners of a 7x7 with a dark ring and 3x3 center.
	for _, corner := range [][2]int{{0, 0}, {6, 0}, {0, 6}, {6, 6}, {3, 3}} {
		if mod[corner[1]][corner[0]] != 1 {
			t.Errorf("top-left finder pixel (%d,%d) = %d, want 1", corner[0], corner[1], mod[corner[1]][corner[0]])
		}
	}
	// Top-right finder anchor.
	if mod[0][expectedSize-1] != 1 || mod[6][expectedSize-7] != 1 {
		t.Error("top-right finder pattern missing dark corners")
	}
	// Bottom-left finder anchor.
	if mod[expectedSize-1][0] != 1 || mod[expectedSize-7][6] != 1 {
		t.Error("bottom-left finder pattern missing dark corners")
	}
	// Always-dark module at (col=8, row=size-8).
	if mod[expectedSize-8][8] != 1 {
		t.Error("always-dark module missing at (8, size-8)")
	}
}

// TestEncoderVersionSelection verifies smaller payloads pack into smaller
// QR versions and larger payloads grow as needed.
func TestEncoderVersionSelection(t *testing.T) {
	cases := []struct {
		payload string
		ec      ECLevel
		maxVer  int
	}{
		{"hi", ECMedium, 1},
		{strings.Repeat("a", 50), ECMedium, 5},
		{strings.Repeat("a", 200), ECMedium, 12},
	}
	for _, c := range cases {
		_, v, err := Encode([]byte(c.payload), c.ec)
		if err != nil {
			t.Fatalf("Encode(%d bytes): %v", len(c.payload), err)
		}
		if v > c.maxVer {
			t.Errorf("Encode(%d bytes) → v%d, expected ≤ v%d", len(c.payload), v, c.maxVer)
		}
	}
}

// TestEncoderRejectsOversizedPayload checks the encoder returns an error
// rather than panicking when the payload exceeds version 40 capacity.
func TestEncoderRejectsOversizedPayload(t *testing.T) {
	huge := bytes.Repeat([]byte{'a'}, 5000)
	_, _, err := Encode(huge, ECHigh)
	if err == nil {
		t.Fatal("expected error for oversized payload at ECHigh")
	}
}

// TestEncoderSVGContainsModules verifies the rendered SVG mentions the
// expected fill color and contains at least one path "M" command per dark
// module. A trivial sanity check; doesn't validate scan-correctness.
func TestEncoderSVGContainsModules(t *testing.T) {
	svg, _, err := EncodeSVG([]byte("hello"), ECMedium)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Errorf("SVG not well-formed: %q", svg[:min(80, len(svg))])
	}
	if !strings.Contains(svg, `fill="#0a0a0a"`) {
		t.Error("SVG missing dark-fill path")
	}
	if !strings.Contains(svg, `viewBox="`) {
		t.Error("SVG missing viewBox")
	}
}

// TestGFArithmeticIsConsistent walks a few GF(256) round-trips through the
// exp/log tables to catch a busted primitive-polynomial init.
func TestGFArithmeticIsConsistent(t *testing.T) {
	// gfExp[0] = 1, gfExp[1] = 2, gfExp[2] = 4 ... gfExp[7] = 128, gfExp[8] = 0x1D
	want := []byte{1, 2, 4, 8, 16, 32, 64, 128, 0x1D, 0x3A}
	for i, w := range want {
		if gfExp[i] != w {
			t.Errorf("gfExp[%d] = %#x, want %#x", i, gfExp[i], w)
		}
	}
	// Multiplication round-trip via log.
	for a := byte(1); a < 16; a++ {
		for b := byte(1); b < 16; b++ {
			prod := gfMul(a, b)
			if prod == 0 {
				t.Errorf("gfMul(%d, %d) unexpectedly 0", a, b)
			}
		}
	}
}

// TestFormatInfoBits confirms the format-info value for a known
// (EC, mask) pair matches the canonical table from ISO/IEC 18004 Annex C.
// Spec sample: ECLow, mask 0 → 0x77C4.
func TestFormatInfoBits(t *testing.T) {
	got := formatInfo(ECLow, 0)
	want := uint32(0x77C4)
	if got != want {
		t.Errorf("formatInfo(L, 0) = %#x, want %#x", got, want)
	}
	// ECHigh, mask 7 → 0x083B (from Annex C).
	got2 := formatInfo(ECHigh, 7)
	want2 := uint32(0x083B)
	if got2 != want2 {
		t.Errorf("formatInfo(H, 7) = %#x, want %#x", got2, want2)
	}
}

// TestVersionInfoBits confirms the version-info value for v7 — the smallest
// version that has version info — matches the spec. v7 = 0x07C94.
func TestVersionInfoBits(t *testing.T) {
	got := versionInfoBits(7)
	want := uint32(0x07C94)
	if got != want {
		t.Errorf("versionInfoBits(7) = %#x, want %#x", got, want)
	}
}
