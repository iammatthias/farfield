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

// Two images with emphasis around them both draw: styling a later line must
// not overwrite an earlier image's placement (they once shared memory).
func TestImagesSurviveEmphasis(t *testing.T) {
	e := newEd(t)
	e.setText("*one* **two**\n![](blob://a)\n_three_ and **four**\n![](blob://b)\n*five*")
	put := func(url string, r, g, b byte) {
		const w, h = 30, 12
		ptr := uint32(e.call("image_alloc", w*h*4))
		px := make([]byte, w*h*4)
		for i := 0; i < len(px); i += 4 {
			px[i], px[i+1], px[i+2], px[i+3] = r, g, b, 255
		}
		e.m.Memory().Write(ptr, px)
		e.m.Memory().Write(uint32(e.call("io_ptr")), []byte(url))
		e.call("image_put", uint64(len(url)), w, h, uint64(ptr))
	}
	put("blob://a", 250, 10, 10)
	put("blob://b", 10, 250, 10)
	e.call("render")
	img := e.frame()
	has := func(r, g, b uint8) bool {
		bd := img.Bounds()
		for y := bd.Min.Y; y < bd.Max.Y; y++ {
			for x := bd.Min.X; x < bd.Max.X; x++ {
				c := img.RGBAAt(x, y)
				if c.R == r && c.G == g && c.B == b {
					return true
				}
			}
		}
		return false
	}
	if !has(250, 10, 10) || !has(10, 250, 10) {
		t.Fatalf("both images should draw (a=%v b=%v)", has(250, 10, 10), has(10, 250, 10))
	}
}

// Raw HTML / JSX lines are markup: every byte is code-styled and dimmed, and
// no inline Markdown runs inside them.
func TestHTMLLinesAreMarkup(t *testing.T) {
	e := newEd(t)
	src := "<div style=\"display:flex\">\n\t<StripeButton id=\"x_y_z\" />\n</div>\n*prose*"
	e.setText(src)
	e.call("render")
	styles, _ := e.m.Memory().Read(0x00DD0000, uint32(len(src)))
	for i := 0; i < strings.Index(src, "\n*prose*"); i++ {
		if src[i] == '\n' || src[i] == '\t' {
			continue
		}
		if styles[i]&4 == 0 || styles[i]&16 == 0 {
			t.Fatalf("byte %d %q of an HTML line has style %#x, want code|mark", i, src[i], styles[i])
		}
		if styles[i]&2 != 0 {
			t.Fatalf("emphasis ran inside HTML at byte %d", i)
		}
	}
	p := strings.Index(src, "prose")
	if styles[p]&2 == 0 {
		t.Fatalf("the prose line after the HTML should still be italic")
	}
}

// A backslash escape dims the backslash and keeps the next character literal:
// it neither starts emphasis nor looks like syntax itself.
func TestBackslashEscapes(t *testing.T) {
	e := newEd(t)
	src := `a \_not italic\_ and \[not a link\](x) *yes*`
	e.setText(src)
	e.call("render")
	styles, _ := e.m.Memory().Read(0x00DD0000, uint32(len(src)))
	bs := strings.Index(src, `\_`)
	if styles[bs]&16 == 0 {
		t.Fatalf("the backslash should be dimmed syntax")
	}
	if styles[bs+1]&16 != 0 {
		t.Fatalf("the escaped character should read as plain text")
	}
	n := strings.Index(src, "not italic")
	if styles[n]&2 != 0 {
		t.Fatalf("escaped underscores must not start emphasis")
	}
	l := strings.Index(src, "not a link")
	if styles[l]&8 != 0 {
		t.Fatalf("an escaped bracket must not start a link")
	}
	y := strings.Index(src, "yes")
	if styles[y]&2 == 0 {
		t.Fatalf("real emphasis after the escapes should still work")
	}
}

// A caret blink repaints one line's strip, not the whole surface.
func TestBlinkRepaintsOnlyTheCaretLine(t *testing.T) {
	e := newEd(t)
	e.setText("one\ntwo\nthree\nfour\nfive")
	e.call("tick", 0)
	e.call("render")
	if e.call("dirty_h") != 700 {
		t.Fatalf("a full render should mark the whole surface dirty, got %d", e.call("dirty_h"))
	}
	e.call("tick", 5000) // long after the last edit: somewhere in the blink cycle
	e.call("tick", 5700)
	if e.call("render") == 0 {
		t.Fatal("a blink should repaint something")
	}
	if h := e.call("dirty_h"); h == 0 || h >= 700 {
		t.Fatalf("blink dirtied %d rows; want one line's strip", h)
	}
}

// Markdown syntax is concealed on lines away from the selection — a link
// shows only its text, a hard-break backslash vanishes — and comes back when
// the caret reaches the line.
func TestSyntaxConcealedAwayFromCaret(t *testing.T) {
	e := newEd(t)
	src := "[PROJECT](https://pure---internet.com) and more\n- Bard\\\nlast line"
	e.setText(src)
	e.call("render")
	styles, _ := e.m.Memory().Read(0x00DD0000, uint32(len(src)))
	if styles[strings.Index(src, "https")]&16 == 0 {
		t.Fatal("a link's destination should be syntax")
	}
	if styles[strings.Index(src, "Bard\\")+4]&16 == 0 {
		t.Fatal("a line-ending backslash should be syntax")
	}
	// the right edge of ink in the first line's rows
	inkRight := func() int {
		img := e.frame()
		bg := img.RGBAAt(img.Bounds().Max.X-1, img.Bounds().Max.Y-1)
		right := 0
		for y := 60; y < 110; y++ { // the first line's band at dpr 2
			for x := 0; x < img.Bounds().Max.X; x++ {
				if img.RGBAAt(x, y) != bg && x > right {
					right = x
				}
			}
		}
		return right
	}
	e.sel(0, 0) // caret on line one: the URL shows
	e.call("render")
	shown := inkRight()
	e.sel(len(src), len(src)) // caret on the last line: line one is concealed
	e.call("render")
	hidden := inkRight()
	if hidden >= shown {
		t.Fatalf("line one should shrink when concealed: ink reaches %d, shown %d", hidden, shown)
	}
}

// A band render draws exactly what a full render draws in those rows.
func TestRenderBandMatchesFullRender(t *testing.T) {
	e := newEd(t)
	e.call("set_page", 1)
	e.call("resize", 900, 700, 200)
	e.setText(PreviewSample)
	e.call("render")
	full := e.frame()
	// scribble over the framebuffer, then redraw a band
	fb := uint32(e.call("fb_ptr"))
	junk := make([]byte, 900*700*4)
	e.m.Memory().Write(fb, junk)
	if e.call("render_band", 150, 400) == 0 {
		t.Fatal("render_band drew nothing")
	}
	y0, h := int(e.call("dirty_y")), int(e.call("dirty_h"))
	if y0 > 150 || y0+h < 400 {
		t.Fatalf("band %d..%d should cover 150..400", y0, y0+h)
	}
	band := e.frame()
	for y := y0; y < y0+h; y++ {
		for x := 0; x < 900; x++ {
			if band.RGBAAt(x, y) != full.RGBAAt(x, y) {
				t.Fatalf("pixel %d,%d differs: band %v full %v", x, y, band.RGBAAt(x, y), full.RGBAAt(x, y))
			}
		}
	}
}

// Spelling: misspellings get a red wave; names, code, links, URLs, HTML and
// identifiers never do, and the personal dictionary clears a word.
func TestSpellingUnderlines(t *testing.T) {
	e := newEd(t)
	words := "the\ncat\nsat\non\nmat\nwith\nand\na\nlink\nsome\ncode\nhere\n"
	e.m.Memory().Write(uint32(e.call("io_ptr")), []byte(words))
	if n := e.call("dict_load", uint64(len(words))); n == 0 {
		t.Fatal("dict_load loaded nothing")
	}
	reds := func() int {
		img := e.frame()
		n := 0
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := img.RGBAAt(x, y)
				if c.R == 0xA6 && c.G == 0x2A && c.B == 0x20 {
					n++
				}
			}
		}
		return n
	}
	// nothing here is a misspelling, by the rules
	clean := "the cat sat on the mat with Ethereum and GPT\n" +
		"some `cdoe` here and a [lnik](https://exmaple.com/pth)\n" +
		"<div clss=\"wrapr\">\n" +
		"the snake_cse and foo.bar and v2beta\n"
	e.setText(clean)
	e.sel(len(clean), len(clean))
	e.call("render")
	if n := reds(); n != 0 {
		t.Fatalf("nothing should be flagged, but %d red pixels were drawn", n)
	}
	// one real typo
	src := clean + "the cat sat on teh mat\nend"
	e.setText(src)
	e.sel(len(src), len(src))
	e.call("render")
	if reds() == 0 {
		t.Fatal("teh should be underlined")
	}
	e.m.Memory().Write(uint32(e.call("io_ptr")), []byte("teh"))
	if e.call("dict_has", 3) != 0 {
		t.Fatal("dict_has should not know teh yet")
	}
	e.call("dict_add", 3)
	e.call("render")
	if n := reds(); n != 0 {
		t.Fatalf("after adding teh, %d red pixels remain", n)
	}
}
