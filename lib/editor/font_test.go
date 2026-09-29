package editor

import (
	"context"
	"image"
	"math"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// newModule assembles and instantiates the editor.
func newModule(t testing.TB) api.Module {
	t.Helper()
	bin, err := Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	t.Cleanup(func() { rt.Close(ctx) })
	m, err := rt.Instantiate(ctx, bin)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	return m
}

func callT(t testing.TB, m api.Module, name string, args ...uint64) uint64 {
	t.Helper()
	f := m.ExportedFunction(name)
	if f == nil {
		t.Fatalf("no export %s", name)
	}
	out, err := f.Call(context.Background(), args...)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(out) == 0 {
		return 0
	}
	return out[0]
}

func loadFont(t testing.TB, m api.Module, slot uint32, ttf []byte) {
	t.Helper()
	io := uint32(callT(t, m, "io_ptr"))
	if !m.Memory().Write(io, ttf) {
		t.Fatal("font does not fit IO")
	}
	if callT(t, m, "font_load", uint64(slot), uint64(len(ttf))) != 1 {
		t.Fatalf("font_load slot %d failed", slot)
	}
}

// glyphEntry reads a glyph cache entry: bitmap, width, height, left, top.
func glyphEntry(t testing.TB, m api.Module, slot uint32, r rune, px int) (bm []byte, w, h, left, top int) {
	t.Helper()
	e := uint32(callT(t, m, "glyph_debug", uint64(slot), uint64(r), uint64(px)))
	mem := m.Memory()
	addr, _ := mem.ReadUint32Le(e + 4)
	uw, _ := mem.ReadUint32Le(e + 8)
	uh, _ := mem.ReadUint32Le(e + 12)
	ul, _ := mem.ReadUint32Le(e + 16)
	ut, _ := mem.ReadUint32Le(e + 20)
	w, h, left, top = int(uw), int(uh), int(int32(ul)), int(int32(ut))
	bm, _ = mem.Read(addr, uint32(w*h))
	return append([]byte(nil), bm...), w, h, left, top
}

// reference rasterizes the same glyph with golang.org/x/image/vector into a
// box of the same geometry, for comparison.
func reference(t *testing.T, f *sfnt.Font, r rune, px, w, h, left, top int) []byte {
	t.Helper()
	var buf sfnt.Buffer
	gi, err := f.GlyphIndex(&buf, r)
	if err != nil {
		t.Fatal(err)
	}
	segs, err := f.LoadGlyph(&buf, gi, fixed.I(px), nil)
	if err != nil {
		t.Fatal(err)
	}
	z := vector.NewRasterizer(w, h)
	tf := func(p fixed.Point26_6) (float32, float32) {
		// sfnt yields y down from the baseline; the box's baseline is at row top
		return float32(p.X)/64 - float32(left), float32(p.Y)/64 + float32(top)
	}
	for _, s := range segs {
		switch s.Op {
		case sfnt.SegmentOpMoveTo:
			z.MoveTo(tf(s.Args[0]))
		case sfnt.SegmentOpLineTo:
			z.LineTo(tf(s.Args[0]))
		case sfnt.SegmentOpQuadTo:
			x1, y1 := tf(s.Args[0])
			x2, y2 := tf(s.Args[1])
			z.QuadTo(x1, y1, x2, y2)
		case sfnt.SegmentOpCubeTo:
			x1, y1 := tf(s.Args[0])
			x2, y2 := tf(s.Args[1])
			x3, y3 := tf(s.Args[2])
			z.CubeTo(x1, y1, x2, y2, x3, y3)
		}
	}
	dst := image.NewAlpha(image.Rect(0, 0, w, h))
	z.Draw(dst, dst.Bounds(), image.Opaque, image.Point{})
	return dst.Pix
}

func ascii(bm []byte, w, h int) string {
	var b strings.Builder
	ramp := " .:-=+*#%@"
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			b.WriteByte(ramp[int(bm[y*w+x])*9/255])
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// TestRasterizerMatchesReference renders glyphs with the hand-written
// rasterizer and compares them to x/image/vector's rendering of the same
// outline: they must agree to within a small mean per-pixel difference.
func TestRasterizerMatchesReference(t *testing.T) {
	m := newModule(t)
	loadFont(t, m, 0, goregular.TTF)
	f, err := sfnt.Parse(goregular.TTF)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []rune("agOQ&é@%W—") {
		for _, px := range []int{14, 34, 72} {
			bm, w, h, left, top := glyphEntry(t, m, 0, r, px)
			if w == 0 || h == 0 {
				t.Errorf("%q@%d: empty bitmap", r, px)
				continue
			}
			ref := reference(t, f, r, px, w, h, left, top)
			var diff, ink float64
			for i := range bm {
				diff += math.Abs(float64(bm[i]) - float64(ref[i]))
				ink += float64(ref[i])
			}
			mean := diff / float64(len(bm))
			if ink == 0 {
				t.Errorf("%q@%d: reference is empty", r, px)
				continue
			}
			// relative to the ink, the disagreement must be small
			if rel := diff / ink; rel > 0.08 {
				t.Errorf("%q@%d: differs from reference by %.1f%% of ink (mean %.1f)\nours:\n%sref:\n%s",
					r, px, rel*100, mean, ascii(bm, w, h), ascii(ref, w, h))
			}
		}
	}
}

func TestSpaceHasNoBitmap(t *testing.T) {
	m := newModule(t)
	loadFont(t, m, 0, goregular.TTF)
	_, w, h, _, _ := glyphEntry(t, m, 0, ' ', 32)
	if w != 0 || h != 0 {
		t.Errorf("space bitmap %dx%d", w, h)
	}
}

func TestFontLoadRejectsGarbage(t *testing.T) {
	m := newModule(t)
	io := uint32(callT(t, m, "io_ptr"))
	m.Memory().Write(io, []byte("definitely not a font file at all"))
	if callT(t, m, "font_load", 0, 34) != 0 {
		t.Error("garbage accepted as a font")
	}
}
