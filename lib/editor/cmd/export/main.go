// Command export writes the editor's runtime assets to a directory, for hosts
// that are not Go: the assembled editor.wasm, the font slots in load order,
// the spelling dictionary, and a manifest naming them. The native client in
// clients/ builds from this, so it runs the exact bytes the browser and desk
// run rather than a second assembly of the same source.
//
//	go run ./lib/editor/cmd/export -out clients/target/editor
package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/iammatthias/farfield/lib/editor"
)

type manifest struct {
	Version string   `json:"version"`
	Wasm    string   `json:"wasm"`
	Fonts   []string `json:"fonts"` // slot order
	Dict    string   `json:"dict"`
}

func main() {
	out := flag.String("out", "", "directory to write the assets into")
	flag.Parse()
	if *out == "" {
		log.Fatal("export: -out is required")
	}
	bin, err := editor.Wasm()
	if err != nil {
		log.Fatal(err)
	}
	m := manifest{Version: editor.Version(), Wasm: "editor.wasm", Dict: "en_US.txt"}
	write(*out, m.Wasm, bin)
	write(*out, m.Dict, editor.Dictionary)
	for _, name := range editor.FontSlots {
		b, err := editor.Font(name)
		if err != nil {
			log.Fatal(err)
		}
		write(*out, filepath.Join("fonts", name), b)
		m.Fonts = append(m.Fonts, filepath.Join("fonts", name))
	}
	j, _ := json.MarshalIndent(m, "", "  ")
	write(*out, "manifest.json", append(j, '\n'))
}

// write replaces a file only when its bytes changed, so a build system
// watching mtimes does not rebuild for an identical export.
func write(dir, name string, b []byte) {
	p := filepath.Join(dir, name)
	if old, err := os.ReadFile(p); err == nil && string(old) == string(b) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		log.Fatal(err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		log.Fatal(err)
	}
}
