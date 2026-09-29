// Command desk is farfield's editor as a desktop app: the same hand-written
// editor.wasm the browser runs, executed natively by wazero, in a window
// Ebitengine provides. The editor draws every pixel; this program only moves
// input in and the framebuffer out.
//
//	desk notes.md      open (or create) a Markdown file; ⌘S saves it
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"

	"github.com/iammatthias/farfield/lib/editor"
)

type app struct {
	ed      *editor.Engine
	path    string
	saved   uint32 // editor revision at the last save
	surface *ebiten.Image
	w, h    int
	scale   float64
	start   time.Time
	// pointer state
	down      bool
	lastClick time.Time
	clicks    int
	lastX     int
	lastY     int
	title     string
}

// keyMap pairs Ebitengine keys with the editor's key codes.
var keyMap = map[ebiten.Key]int{
	ebiten.KeyArrowLeft: editor.KeyLeft, ebiten.KeyArrowRight: editor.KeyRight,
	ebiten.KeyArrowUp: editor.KeyUp, ebiten.KeyArrowDown: editor.KeyDown,
	ebiten.KeyHome: editor.KeyHome, ebiten.KeyEnd: editor.KeyEnd,
	ebiten.KeyPageUp: editor.KeyPageUp, ebiten.KeyPageDown: editor.KeyPageDown,
	ebiten.KeyBackspace: editor.KeyBackspace, ebiten.KeyDelete: editor.KeyDelete,
	ebiten.KeyEnter: editor.KeyEnter, ebiten.KeyNumpadEnter: editor.KeyEnter,
	ebiten.KeyTab: editor.KeyTab, ebiten.KeyEscape: editor.KeyEscape,
}

var mac = runtime.GOOS == "darwin"

func (a *app) mods() int {
	m := 0
	if ebiten.IsKeyPressed(ebiten.KeyShift) {
		m |= editor.ModShift
	}
	if (mac && ebiten.IsKeyPressed(ebiten.KeyAlt)) || (!mac && ebiten.IsKeyPressed(ebiten.KeyControl)) {
		m |= editor.ModWord
	}
	if a.primary() {
		m |= editor.ModCmd
	}
	return m
}

func (a *app) primary() bool {
	if mac {
		return ebiten.IsKeyPressed(ebiten.KeyMeta)
	}
	return ebiten.IsKeyPressed(ebiten.KeyControl)
}

// repeat reports a key press or, while held, an auto-repeat.
func repeat(k ebiten.Key) bool {
	d := inpututil.KeyPressDuration(k)
	return d == 1 || (d >= 30 && (d-30)%3 == 0)
}

func (a *app) Update() error {
	a.ed.Tick(int(time.Since(a.start).Milliseconds()))

	// shortcuts
	if a.primary() {
		shift := ebiten.IsKeyPressed(ebiten.KeyShift)
		alt := ebiten.IsKeyPressed(ebiten.KeyAlt)
		just := inpututil.IsKeyJustPressed
		switch {
		case just(ebiten.KeyS):
			a.save()
		case just(ebiten.KeyZ) && shift, just(ebiten.KeyY):
			a.ed.Command(editor.CmdRedo)
		case just(ebiten.KeyZ):
			a.ed.Command(editor.CmdUndo)
		case just(ebiten.KeyB):
			a.ed.Command(editor.CmdBold)
		case just(ebiten.KeyI):
			a.ed.Command(editor.CmdItalic)
		case just(ebiten.KeyE):
			a.ed.Command(editor.CmdCode)
		case just(ebiten.KeyK):
			a.ed.Command(editor.CmdLink)
		case just(ebiten.KeyA):
			a.ed.Command(editor.CmdSelectAll)
		case just(ebiten.KeyX) && shift:
			a.ed.Command(editor.CmdStrike)
		case just(ebiten.Key1) && alt:
			a.ed.Command(editor.CmdH1)
		case just(ebiten.Key2) && alt:
			a.ed.Command(editor.CmdH2)
		case just(ebiten.Key3) && alt:
			a.ed.Command(editor.CmdH3)
		case just(ebiten.Key7) && shift:
			a.ed.Command(editor.CmdNumbers)
		case just(ebiten.Key8) && shift:
			a.ed.Command(editor.CmdBullets)
		case just(ebiten.Key9) && shift:
			a.ed.Command(editor.CmdQuote)
		case just(ebiten.KeyC):
			if sel := a.ed.Selection(); sel != "" {
				copyText(sel)
			}
		case just(ebiten.KeyX):
			if sel := a.ed.Selection(); sel != "" {
				copyText(sel)
				a.ed.Key(editor.KeyBackspace, 0)
			}
		case just(ebiten.KeyV):
			if s := pasteText(); s != "" {
				a.ed.Insert(strings.ReplaceAll(s, "\r\n", "\n"))
			}
		}
	} else {
		// typed characters
		if chars := ebiten.AppendInputChars(nil); len(chars) > 0 {
			a.ed.Insert(string(chars))
		}
	}
	for k, code := range keyMap {
		if repeat(k) {
			a.ed.Key(code, a.mods())
		}
	}

	// pointer
	x, y := ebiten.CursorPosition()
	px, py := int(float64(x)*a.scale), int(float64(y)*a.scale)
	if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft) {
		if a.primary() {
			if link := a.ed.LinkAt(px, py); link != "" {
				openURL(link)
			}
		}
		now := time.Now()
		if now.Sub(a.lastClick) < 400*time.Millisecond && abs(x-a.lastX) < 4 && abs(y-a.lastY) < 4 {
			a.clicks = min(a.clicks+1, 3)
		} else {
			a.clicks = 1
		}
		a.lastClick, a.lastX, a.lastY = now, x, y
		a.down = true
		a.ed.Pointer(1, px, py, a.mods(), a.clicks)
	} else if a.down && ebiten.IsMouseButtonPressed(ebiten.MouseButtonLeft) {
		a.ed.Pointer(2, px, py, a.mods(), 0)
	} else if a.down {
		a.down = false
		a.ed.Pointer(3, 0, 0, 0, 0)
	}
	if _, dy := ebiten.Wheel(); dy != 0 {
		a.ed.Wheel(int(-dy * 40 * a.scale))
	}

	a.updateTitle()
	return nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func (a *app) updateTitle() {
	t := filepath.Base(a.path)
	if a.ed.Revision() != a.saved {
		t = "• " + t
	}
	t += fmt.Sprintf(" — %d words", a.ed.Words())
	if t != a.title {
		a.title = t
		ebiten.SetWindowTitle(t)
	}
}

func (a *app) save() {
	if err := os.WriteFile(a.path, []byte(a.ed.Text()), 0o644); err != nil {
		log.Printf("save: %v", err)
		return
	}
	a.saved = a.ed.Revision()
}

func (a *app) Draw(screen *ebiten.Image) {
	fb, _ := a.ed.Render()
	// the editor's framebuffer is already RGBA in the right size
	a.surface.WritePixels(fb)
	screen.DrawImage(a.surface, nil)
}

func (a *app) Layout(outsideW, outsideH int) (int, int) {
	s := ebiten.Monitor().DeviceScaleFactor()
	w, h := int(float64(outsideW)*s), int(float64(outsideH)*s)
	if w != a.w || h != a.h || s != a.scale {
		a.w, a.h, a.scale = w, h, s
		if err := a.ed.Resize(w, h, s); err != nil {
			log.Fatal(err)
		}
		a.surface = ebiten.NewImage(w, h)
	}
	return w, h
}

// Clipboard through the platform's own tools, which keeps this program free
// of cgo.
func copyText(s string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "windows":
		cmd = exec.Command("clip")
	default:
		cmd = exec.Command("wl-copy")
	}
	cmd.Stdin = strings.NewReader(s)
	_ = cmd.Run()
}

func pasteText() string {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbpaste")
	case "windows":
		cmd = exec.Command("powershell", "-NoProfile", "-Command", "Get-Clipboard")
	default:
		cmd = exec.Command("wl-paste", "--no-newline")
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func openURL(u string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", u).Start()
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		_ = exec.Command("xdg-open", u).Start()
	}
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: desk <file.md>")
		os.Exit(2)
	}
	path := os.Args[1]
	body, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Fatal(err)
	}

	ed, err := editor.NewEngine(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	defer ed.Close()
	ed.Placeholder("Write something…")
	ed.SetText(string(body))
	ed.Focus(true)

	a := &app{ed: ed, path: path, saved: ed.Revision(), start: time.Now()}
	ebiten.SetWindowSize(960, 820)
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	ebiten.SetScreenClearedEveryFrame(false)
	ebiten.SetTPS(60)
	a.updateTitle()
	if err := ebiten.RunGame(a); err != nil {
		log.Fatal(err)
	}
}
