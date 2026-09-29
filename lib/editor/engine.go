package editor

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Key codes and modifier bits, as edit.wat defines them.
const (
	KeyLeft = iota + 1
	KeyRight
	KeyUp
	KeyDown
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyBackspace
	KeyDelete
	KeyEnter
	KeyTab
	KeyEscape
)

const (
	ModShift = 1
	ModWord  = 2 // ⌥ on a Mac, Ctrl elsewhere
	ModCmd   = 4 // ⌘ on a Mac, Ctrl elsewhere
)

// Command ids, as edit.wat defines them.
const (
	CmdBold = iota + 1
	CmdItalic
	CmdCode
	CmdLink
	CmdStrike
	CmdH1
	CmdH2
	CmdH3
	CmdQuote
	CmdBullets
	CmdNumbers
	CmdCodeBlock
	CmdUndo
	CmdRedo
	CmdSelectAll
	CmdRule
	CmdSelectWord
	CmdSelectLine
)

// Engine runs the editor module natively under wazero: the same bytes the
// browser runs, driven from Go — the desktop host, and anything else that
// wants the editor without a browser.
type Engine struct {
	ctx context.Context
	rt  wazero.Runtime
	m   api.Module
	fns map[string]api.Function
	w   int
	h   int
}

// NewEngine instantiates the editor and loads the brand fonts.
func NewEngine(ctx context.Context) (*Engine, error) {
	bin, err := Wasm()
	if err != nil {
		return nil, err
	}
	rt := wazero.NewRuntime(ctx)
	m, err := rt.Instantiate(ctx, bin)
	if err != nil {
		rt.Close(ctx)
		return nil, err
	}
	e := &Engine{ctx: ctx, rt: rt, m: m, fns: map[string]api.Function{}}
	e.call("init")
	for slot, name := range FontSlots {
		b, err := Font(name)
		if err != nil {
			e.Close()
			return nil, err
		}
		if !e.m.Memory().Write(e.io(), b) || e.call("font_load", uint64(slot), uint64(len(b))) != 1 {
			e.Close()
			return nil, fmt.Errorf("editor: font %s did not load", name)
		}
	}
	return e, nil
}

// Close releases the runtime.
func (e *Engine) Close() error { return e.rt.Close(e.ctx) }

func (e *Engine) call(name string, args ...uint64) uint64 {
	f, ok := e.fns[name]
	if !ok {
		f = e.m.ExportedFunction(name)
		if f == nil {
			panic("editor: no export " + name)
		}
		e.fns[name] = f
	}
	out, err := f.Call(e.ctx, args...)
	if err != nil {
		panic(fmt.Sprintf("editor: %s: %v", name, err))
	}
	if len(out) == 0 {
		return 0
	}
	return out[0]
}

func (e *Engine) io() uint32 { return uint32(e.call("io_ptr")) }

func (e *Engine) put(b []byte) uint64 {
	e.m.Memory().Write(e.io(), b)
	return uint64(len(b))
}

func (e *Engine) take(n uint64) string {
	b, _ := e.m.Memory().Read(e.io(), uint32(n))
	return string(b)
}

// Resize sets the surface in device pixels and the scale (1.0 = 100).
func (e *Engine) Resize(w, h int, scale float64) error {
	if e.call("resize", uint64(w), uint64(h), uint64(scale*100+0.5)) == 0 {
		return errors.New("editor: could not grow memory for the framebuffer")
	}
	e.w, e.h = w, h
	return nil
}

// Render draws if anything changed and returns the RGBA framebuffer (a view
// into the module's memory, valid until the next call) and whether it drew.
func (e *Engine) Render() ([]byte, bool) {
	drew := e.call("render") == 1
	fb, _ := e.m.Memory().Read(uint32(e.call("fb_ptr")), uint32(e.w*e.h*4))
	return fb, drew
}

func (e *Engine) SetText(s string)  { e.call("set_text", e.put([]byte(s))) }
func (e *Engine) Text() string      { return e.take(e.call("get_text")) }
func (e *Engine) Selection() string { return e.take(e.call("get_selection")) }
func (e *Engine) Insert(s string)   { e.call("insert_text", e.put([]byte(s))) }
func (e *Engine) Placeholder(s string) {
	e.call("set_placeholder", e.put([]byte(s)))
}
func (e *Engine) Key(k, mods int) bool { return e.call("key", uint64(k), uint64(mods)) == 1 }
func (e *Engine) Command(c int)        { e.call("command", uint64(c)) }
func (e *Engine) Pointer(kind, x, y, mods, clicks int) {
	e.call("pointer", uint64(kind), uint64(x), uint64(y), uint64(mods), uint64(clicks))
}
func (e *Engine) Wheel(dy int)     { e.call("wheel", uint64(uint32(int32(dy)))) }
func (e *Engine) Focus(on bool)    { e.call("focus", b2u(on)) }
func (e *Engine) Tick(ms int)      { e.call("tick", uint64(uint32(ms))) }
func (e *Engine) Revision() uint32 { return uint32(e.call("revision")) }
func (e *Engine) Words() int       { return int(e.call("word_count")) }
func (e *Engine) SetColor(i int, rgba uint32) {
	e.call("set_color", uint64(i), uint64(rgba))
}

// LinkAt returns the destination of a link under a surface point, or "".
func (e *Engine) LinkAt(x, y int) string {
	return e.take(e.call("link_at", uint64(x), uint64(y)))
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}
