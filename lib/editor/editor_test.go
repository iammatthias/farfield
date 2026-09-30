package editor

import (
	"image"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gobolditalic"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/goregular"
)

// ed wraps an instantiated editor for tests.
type ed struct {
	t testing.TB
	m api.Module
}

const (
	kLeft = iota + 1
	kRight
	kUp
	kDown
	kHome
	kEnd
	kPageUp
	kPageDown
	kBackspace
	kDelete
	kEnter
	kTab
	kEscape
)

const (
	mShift = 1
	mWord  = 2
	mCmd   = 4
)

const (
	cBold = iota + 1
	cItalic
	cCode
	cLink
	cStrike
	cH1
	cH2
	cH3
	cQuote
	cBullets
	cNumbers
	cCodeBlock
	cUndo
	cRedo
	cSelectAll
	cRule
	cSelectWord
	cSelectLine
)

func newEd(t testing.TB) *ed {
	t.Helper()
	m := newModule(t)
	e := &ed{t: t, m: m}
	callT(t, m, "init")
	for slot, ttf := range [][]byte{goregular.TTF, gobold.TTF, goitalic.TTF, gobolditalic.TTF, gomono.TTF, gomonobold.TTF, goregular.TTF} {
		loadFont(t, m, uint32(slot), ttf)
	}
	if callT(t, m, "resize", 900, 700, 200) == 0 {
		t.Fatal("resize failed")
	}
	callT(t, m, "focus", 1)
	return e
}

func (e *ed) call(name string, args ...uint64) uint64 { return callT(e.t, e.m, name, args...) }

func (e *ed) setText(s string) {
	io := uint32(e.call("io_ptr"))
	e.m.Memory().Write(io, []byte(s))
	e.call("set_text", uint64(len(s)))
}

func (e *ed) text() string {
	n := uint32(e.call("get_text"))
	b, _ := e.m.Memory().Read(uint32(e.call("io_ptr")), n)
	return string(b)
}

func (e *ed) selection() string {
	n := uint32(e.call("get_selection"))
	b, _ := e.m.Memory().Read(uint32(e.call("io_ptr")), n)
	return string(b)
}

func (e *ed) typ(s string) {
	// one character at a time, as a keyboard would
	for _, r := range s {
		if r == '\n' {
			e.key(kEnter, 0)
			continue
		}
		b := []byte(string(r))
		e.m.Memory().Write(uint32(e.call("io_ptr")), b)
		e.call("insert_text", uint64(len(b)))
	}
}

func (e *ed) paste(s string) {
	e.m.Memory().Write(uint32(e.call("io_ptr")), []byte(s))
	e.call("insert_text", uint64(len(s)))
}

func (e *ed) key(k, mods int)   { e.call("key", uint64(k), uint64(mods)) }
func (e *ed) cmd(c int)         { e.call("command", uint64(c)) }
func (e *ed) sel(a, h int)      { e.call("set_selection", uint64(a), uint64(h)) }
func (e *ed) caret() (int, int) { return int(e.call("sel_start")), int(e.call("sel_end")) }

func (e *ed) want(s string) {
	e.t.Helper()
	if got := e.text(); got != s {
		e.t.Errorf("text = %q\n     want %q", got, s)
	}
}

// frame renders and returns the framebuffer as an image.
func (e *ed) frame() *image.RGBA {
	e.call("render")
	w, h := 900, 700
	fb := uint32(e.call("fb_ptr"))
	pix, ok := e.m.Memory().Read(fb, uint32(w*h*4))
	if !ok {
		e.t.Fatal("framebuffer out of range")
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	copy(img.Pix, pix)
	return img
}

func TestTypingAndDeleting(t *testing.T) {
	e := newEd(t)
	e.typ("hello wörld")
	e.want("hello wörld")
	e.key(kBackspace, 0)
	e.want("hello wörl")
	e.key(kBackspace, mWord)
	e.want("hello ")
	e.key(kLeft, mWord)
	e.key(kDelete, 0)
	e.want("ello ")
	e.key(kEnd, 0)
	e.typ("—ok")
	e.want("ello —ok")
	e.key(kLeft, 0)
	e.key(kLeft, 0)
	e.key(kLeft, 0) // back over a three-byte em dash
	e.typ("x")
	e.want("ello x—ok")
}

func TestUndoGroupsByWord(t *testing.T) {
	e := newEd(t)
	e.typ("one two three")
	e.cmd(cUndo)
	e.want("one two ")
	e.cmd(cUndo)
	e.want("one ")
	e.cmd(cRedo)
	e.want("one two ")
	e.cmd(cUndo)
	e.cmd(cUndo)
	e.want("")
	e.cmd(cUndo) // nothing left: harmless
	e.want("")
	e.cmd(cRedo)
	e.cmd(cRedo)
	e.cmd(cRedo)
	e.want("one two three")
	// a new edit drops the redo tail
	e.cmd(cUndo)
	e.typ("four")
	e.cmd(cRedo)
	e.want("one two four")
}

func TestPasteReplacesSelectionAndUndoesAsOne(t *testing.T) {
	e := newEd(t)
	e.setText("keep this out")
	e.sel(5, 9)
	e.paste("that thing")
	e.want("keep that thing out")
	e.cmd(cUndo)
	e.want("keep this out")
	if a, b := e.caret(); a != 5 || b != 9 {
		t.Errorf("selection after undo = %d..%d, want 5..9", a, b)
	}
}

func TestEnterContinuesLists(t *testing.T) {
	e := newEd(t)
	e.typ("- milk\neggs\n")
	e.want("- milk\n- eggs\n- ")
	e.key(kEnter, 0) // empty item ends the list
	e.want("- milk\n- eggs\n")

	e = newEd(t)
	e.typ("1. one\ntwo\n")
	e.want("1. one\n2. two\n3. ")

	e = newEd(t)
	e.typ("> quoted\nmore")
	e.want("> quoted\n> more")

	e = newEd(t)
	e.setText("```\n  code")
	e.key(kEnd, mCmd)
	e.key(kEnter, 0)
	e.typ("next")
	e.want("```\n  code\n  next")
}

func TestBackspaceRemovesAListMarker(t *testing.T) {
	e := newEd(t)
	e.typ("- item\n")
	e.want("- item\n- ")
	e.key(kBackspace, 0)
	e.want("- item\n")
}

func TestInlineCommandsToggle(t *testing.T) {
	e := newEd(t)
	e.setText("make this bold")
	e.sel(5, 9)
	e.cmd(cBold)
	e.want("make **this** bold")
	if e.selection() != "this" {
		t.Errorf("selection = %q", e.selection())
	}
	e.cmd(cBold)
	e.want("make this bold")
	e.cmd(cItalic)
	e.want("make *this* bold")
	e.cmd(cItalic)
	e.cmd(cCode)
	e.want("make `this` bold")

	e = newEd(t)
	e.cmd(cBold) // nothing selected: a pair with the caret between
	e.typ("x")
	e.want("**x**")
}

func TestLinkCommand(t *testing.T) {
	e := newEd(t)
	e.setText("see the docs")
	e.sel(8, 12)
	e.cmd(cLink)
	e.typ("https://example.com")
	e.want("see the [docs](https://example.com)")

	e = newEd(t)
	e.setText("https://example.com")
	e.sel(0, 19)
	e.cmd(cLink)
	e.typ("label")
	e.want("[label](https://example.com)")
}

func TestBlockCommands(t *testing.T) {
	e := newEd(t)
	e.setText("title\nbody")
	e.sel(0, 0)
	e.cmd(cH2)
	e.want("## title\nbody")
	e.cmd(cH1)
	e.want("# title\nbody")
	e.cmd(cH1)
	e.want("title\nbody")

	e.setText("a\nb\nc")
	e.sel(0, 5)
	e.cmd(cBullets)
	e.want("- a\n- b\n- c")
	e.cmd(cBullets)
	e.want("a\nb\nc")
	e.cmd(cNumbers)
	e.want("1. a\n2. b\n3. c")
	e.cmd(cQuote)
	e.want("> a\n> b\n> c")

	e.setText("x = 1")
	e.sel(0, 5)
	e.cmd(cCodeBlock)
	e.want("```\nx = 1\n```")
}

func TestTabIndents(t *testing.T) {
	e := newEd(t)
	e.setText("- a\n- b")
	e.sel(4, 7)
	e.key(kTab, 0)
	e.want("- a\n  - b")
	e.key(kTab, mShift)
	e.want("- a\n- b")
}

func TestVerticalMotionAndWords(t *testing.T) {
	e := newEd(t)
	e.setText("first line\nsecond line\nthird")
	e.sel(3, 3)
	e.key(kDown, 0)
	if a, _ := e.caret(); a < 11 || a > 17 {
		t.Errorf("down landed at %d", a)
	}
	e.key(kDown, 0)
	e.key(kDown, 0) // past the end: to the end
	if a, _ := e.caret(); a != len("first line\nsecond line\nthird") {
		t.Errorf("down past end = %d", a)
	}
	e.sel(13, 13)
	e.cmd(cSelectWord)
	if e.selection() != "second" {
		t.Errorf("word = %q", e.selection())
	}
	e.cmd(cSelectLine)
	if e.selection() != "second line\n" {
		t.Errorf("line = %q", e.selection())
	}
	e.cmd(cSelectAll)
	e.key(kRight, 0)
	if a, _ := e.caret(); a != len(e.text()) {
		t.Errorf("→ after select-all = %d", a)
	}
}

func TestWordCount(t *testing.T) {
	e := newEd(t)
	e.setText("# The **quick** brown fox — it's here.\n\n- one\n- two")
	if n := e.call("word_count"); n != 8 {
		t.Errorf("word_count = %d, want 8", n)
	}
}

func TestPointerSelects(t *testing.T) {
	e := newEd(t)
	e.setText("hello world, a line of text that is here")
	e.frame()
	// caret rect gives us a point on the first line
	cr := uint32(e.call("caret_rect"))
	mem := e.m.Memory()
	y, _ := mem.ReadUint32Le(cr + 4)
	h, _ := mem.ReadUint32Le(cr + 12)
	x0, _ := mem.ReadUint32Le(cr)
	py := uint64(y + h/2)
	e.call("pointer", 1, uint64(x0)+2, py, 0, 2) // double-click the first word
	if e.selection() != "hello" {
		t.Errorf("double-click selected %q", e.selection())
	}
	e.call("pointer", 3, 0, 0, 0, 0)
	e.call("pointer", 1, uint64(x0)+2, py, 0, 1)
	e.call("pointer", 2, 5000, py, 0, 0) // drag to far right: end of line
	e.call("pointer", 3, 0, 0, 0, 0)
	if e.selection() != e.text() {
		t.Errorf("drag selected %q", e.selection())
	}
}

func TestLongDocumentWrapsAndScrolls(t *testing.T) {
	e := newEd(t)
	e.setText(strings.Repeat("A paragraph that goes on for a while so it has to wrap across the column. ", 400))
	h := e.call("doc_height")
	if h < 5000 {
		t.Errorf("doc_height = %d, want a tall document", h)
	}
	e.key(kEnd, mCmd)
	if e.call("scroll_top") == 0 {
		t.Error("caret at the end did not scroll")
	}
	e.call("wheel", 1<<31-1) // absurd wheel: clamped
	if top := e.call("scroll_top"); top > h {
		t.Errorf("scroll %d past doc %d", top, h)
	}
}

func TestRenderDrawsSomething(t *testing.T) {
	e := newEd(t)
	e.setText(sample)
	e.sel(40, 40)
	img := e.frame()
	bg := img.RGBAAt(1, 1)
	var ink int
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i] != bg.R || img.Pix[i+1] != bg.G || img.Pix[i+2] != bg.B {
			ink++
		}
	}
	if ink < 20000 {
		t.Errorf("only %d non-background pixels", ink)
	}
	if out := os.Getenv("EDITOR_PNG"); out != "" {
		f, _ := os.Create(out)
		png.Encode(f, img)
		f.Close()
	}
}

const sample = "# Field notes\n\nA **bold** claim, an *aside*, some `inline code`, and a [link](https://farfield.systems). ~~Struck~~ through.\n\n## Lists\n\n- milk\n- eggs, *free range*\n  - nested\n1. first\n2. second\n\n> A quote that runs long enough to wrap onto a second line so the bar spans both of them in the column.\n\n```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```\n\n---\n\nPlain text — with “smart quotes”, é, and an emoji 🌊 the font lacks.\n"

// Plain mode (scrap): Markdown is content, not syntax — no list continuation,
// no heading styling, everything in the mono face.
func TestPlainMode(t *testing.T) {
	e := newEd(t)
	e.call("set_mode", 1)
	e.typ("- not a list\n# not a heading")
	e.want("- not a list\n# not a heading")
	img := e.frame()
	if img.Bounds().Dx() == 0 {
		t.Fatal("no frame")
	}
}

func countNot(img *image.RGBA, c [3]byte) int {
	n := 0
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i] != c[0] || img.Pix[i+1] != c[1] || img.Pix[i+2] != c[2] {
			n++
		}
	}
	return n
}

func has(img *image.RGBA, c [3]byte) bool {
	for i := 0; i < len(img.Pix); i += 4 {
		if img.Pix[i] == c[0] && img.Pix[i+1] == c[1] && img.Pix[i+2] == c[2] {
			return true
		}
	}
	return false
}

// An empty document shows its placeholder (a bitwise-and once hid it).
func TestPlaceholderDraws(t *testing.T) {
	e := newEd(t)
	e.m.Memory().Write(uint32(e.call("io_ptr")), []byte("Write something…"))
	e.call("set_placeholder", uint64(len("Write something…")))
	e.call("focus", 0) // no caret: only the placeholder can draw
	img := e.frame()
	bg := img.RGBAAt(1, 1)
	if n := countNot(img, [3]byte{bg.R, bg.G, bg.B}); n < 1000 {
		t.Errorf("placeholder drew %d pixels", n)
	}
}

// Every covered pixel of a glyph reaches the framebuffer — even coverage
// values included (a bitwise-and once dropped them, graining the text).
func TestGlyphPixelsAllDraw(t *testing.T) {
	e := newEd(t)
	e.call("focus", 0)
	e.setText("M")
	img := e.frame()
	bg := img.RGBAAt(1, 1)
	drawn := countNot(img, [3]byte{bg.R, bg.G, bg.B})
	// the same glyph, straight from the cache: slot 0 at the body size (19 css px × 2)
	bm, _, _, _, _ := glyphEntry(t, e.m, 0, 'M', 38)
	covered := 0
	for _, v := range bm {
		if v != 0 {
			covered++
		}
	}
	if drawn != covered {
		t.Errorf("drew %d pixels, glyph covers %d", drawn, covered)
	}
}

// Inline code sits on the code tint.
func TestInlineCodeTint(t *testing.T) {
	e := newEd(t)
	e.call("focus", 0)
	e.setText("some `code` here")
	img := e.frame()
	panel := e.m.Memory()
	c, _ := panel.Read(0x110, 3) // palette slot 4: the code panel
	if !has(img, [3]byte{c[0], c[1], c[2]}) {
		t.Error("no code tint drawn")
	}
}

func TestSelRectAndCaretLine(t *testing.T) {
	e := newEd(t)
	e.setText("first line\n/he")
	e.sel(len(e.text()), len(e.text()))
	n := uint32(e.call("caret_line"))
	b, _ := e.m.Memory().Read(uint32(e.call("io_ptr")), n)
	if string(b) != "/he" || e.call("line_start") != 11 {
		t.Errorf("caret_line = %q start %d", b, e.call("line_start"))
	}
	e.sel(0, 5)
	r := uint32(e.call("sel_rect"))
	mem := e.m.Memory()
	x0, _ := mem.ReadUint32Le(r)
	y0, _ := mem.ReadUint32Le(r + 4)
	x1, _ := mem.ReadUint32Le(r + 8)
	y1, _ := mem.ReadUint32Le(r + 12)
	if !(x1 > x0 && y1 > y0) {
		t.Errorf("sel_rect = %d,%d → %d,%d", x0, y0, x1, y1)
	}
}

// Page mode lays out and renders at any size — including the 1×1 surface a
// hidden page hands it — without hanging.
func TestPageModeSizes(t *testing.T) {
	e := newEd(t)
	e.call("set_page", 1)
	for _, wh := range [][2]uint64{{1, 1}, {2, 240}, {1520, 1200}, {1, 1}, {760, 900}} {
		e.call("resize", wh[0], wh[1], 200)
		e.setText(PreviewSample)
		e.call("render")
		h := e.call("doc_height")
		e.call("set_scroll", h)
		e.call("render")
	}
}

// The browser's order: mounted while hidden (1 px wide), text set, then
// shown at full width.
func TestPageModeShownAfterHidden(t *testing.T) {
	e := newEd(t)
	e.call("set_page", 1)
	e.call("resize", 1, 240, 200)
	e.setText(PreviewSample)
	e.call("render")
	e.call("doc_height")
	e.call("resize", 1520, 1673, 200)
	e.call("doc_height")
	e.call("set_scroll", 0)
	e.call("tick", 100)
	e.call("render")
	e.call("caret_rect")
	e.call("sel_rect")
	e.call("caret_line")
}

// The coverage curve keeps the ends fixed, never decreases, and lifts the
// mid-tones that make antialiased text read thin.
func TestGammaCurve(t *testing.T) {
	e := newEd(t)
	lut, ok := e.m.Memory().Read(0x300, 256)
	if !ok {
		t.Fatal("read curve")
	}
	if lut[0] != 0 || lut[255] != 255 {
		t.Fatalf("ends = %d, %d; want 0, 255", lut[0], lut[255])
	}
	for i := 1; i < 256; i++ {
		if lut[i] < lut[i-1] {
			t.Fatalf("curve falls at %d: %d < %d", i, lut[i], lut[i-1])
		}
	}
	if lut[128] < 150 {
		t.Errorf("mid coverage %d not lifted", lut[128])
	}
}

// An image line makes room for its image, and the renderer draws its pixels
// there; the Markdown above it stays as text.
func TestImageLineDrawsPixels(t *testing.T) {
	e := newEd(t)
	e.setText("before\n![](blob://bafkreitest)\nafter")
	e.call("render")
	h0 := e.call("doc_height")

	const w, h = 40, 20
	ptr := uint32(e.call("image_alloc", w*h*4))
	if ptr == 0 {
		t.Fatal("image_alloc failed")
	}
	px := make([]byte, w*h*4)
	for i := 0; i < len(px); i += 4 {
		px[i], px[i+1], px[i+2], px[i+3] = 200, 30, 40, 255 // RGBA
	}
	e.m.Memory().Write(ptr, px)
	url := "blob://bafkreitest"
	e.m.Memory().Write(uint32(e.call("io_ptr")), []byte(url))
	e.call("image_put", uint64(len(url)), w, h, uint64(ptr))

	e.call("render")
	_ = h0
	// the caret is on the first line, away from the image: only the image shows
	hidden := e.call("doc_height")
	// with the caret on the image line its source comes back above the image
	e.sel(len("before\n")+3, len("before\n")+3)
	e.call("render")
	shown := e.call("doc_height")
	if shown <= hidden {
		t.Fatalf("caret on the image line should reveal its source: height %d → %d", hidden, shown)
	}
	e.sel(0, 0)
	e.call("render")
	if got := e.call("doc_height"); got != hidden {
		t.Fatalf("moving away should hide the source again: height %d, want %d", got, hidden)
	}
	img := e.frame()
	found := false
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y && !found; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			if c.R == 200 && c.G == 30 && c.B == 40 {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("image pixels never reached the framebuffer")
	}
	// the source is still text
	if e.text() != "before\n![](blob://bafkreitest)\nafter" {
		t.Fatalf("text changed: %q", e.text())
	}
}
