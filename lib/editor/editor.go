// Package editor is farfield's text editor: a hand-written WebAssembly module
// (wat/*.wat, assembled by lib/wat) that owns everything a text editor is —
// the document, Markdown styling, font rasterization, layout, rendering into
// its own pixel framebuffer, selection, undo — and a thin host that feeds it
// input and shows its pixels. The browser host is host.js on a <canvas>; the
// same module runs on the desktop under wazero.
package editor

import (
	"embed"
	"io/fs"
	"sort"

	"github.com/iammatthias/farfield/lib/wat"
)

//go:embed wat/*.wat
var watFS embed.FS

// Sources returns the editor's .wat files in assembly order: mem.wat first
// (it declares the memory and the region globals), the rest by name.
func Sources() ([]wat.Source, error) {
	names, err := fs.Glob(watFS, "wat/*.wat")
	if err != nil {
		return nil, err
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == "wat/mem.wat" {
			return true
		}
		if names[j] == "wat/mem.wat" {
			return false
		}
		return names[i] < names[j]
	})
	out := make([]wat.Source, 0, len(names))
	for _, n := range names {
		b, err := watFS.ReadFile(n)
		if err != nil {
			return nil, err
		}
		out = append(out, wat.Source{Name: n, Text: string(b)})
	}
	return out, nil
}

// Build assembles the editor module from its hand-written source.
func Build() ([]byte, error) {
	src, err := Sources()
	if err != nil {
		return nil, err
	}
	return wat.Assemble(src...)
}
