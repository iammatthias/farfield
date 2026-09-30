// Command preview serves the component gallery — lib/theme's ui.css, every
// shared component, and the live editor on a document page — or writes it as
// a single self-contained HTML file, so the redesign can be reviewed before
// any app adopts it.
//
//	go run ./lib/editor/cmd/preview                 serve on localhost:7070
//	go run ./lib/editor/cmd/preview -out page.html  write one file
package main

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"text/template"

	"github.com/iammatthias/farfield/lib/editor"
	"github.com/iammatthias/farfield/lib/theme"
)

//go:embed gallery.html
var galleryHTML string

var gallery = template.Must(template.New("gallery").Parse(galleryHTML))

func main() {
	addr := flag.String("addr", "localhost:7070", "listen address")
	out := flag.String("out", "", "write a self-contained gallery page here and exit")
	fragment := flag.Bool("artifact", false, "with -out: omit the document skeleton (for hosts that add their own)")
	flag.Parse()

	if *out != "" {
		page, err := render(true)
		if err != nil {
			log.Fatal(err)
		}
		if *fragment {
			page = stripSkeleton(page)
		}
		if err := os.WriteFile(*out, page, 0o644); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote %s (%d KB)", *out, len(page)/1024)
		return
	}

	mux := http.NewServeMux()
	mux.Handle("/static/editor/", editor.Handler("/static/editor/"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// re-read on every request while iterating on the design
		page, err := render(false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	log.Printf("gallery on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// render fills the gallery. inline embeds the module and the fonts as base64
// so the page stands alone; otherwise mount.js loads them from /static/editor/.
func render(inline bool) ([]byte, error) {
	assets := ""
	if inline {
		bin, err := editor.Wasm()
		if err != nil {
			return nil, err
		}
		var fonts []string
		for _, n := range editor.FontSlots {
			b, err := editor.Font(n)
			if err != nil {
				return nil, err
			}
			fonts = append(fonts, `b("`+base64.StdEncoding.EncodeToString(b)+`")`)
		}
		assets = fmt.Sprintf(`<script>(function () {
	function b(s) { var bin = atob(s), u = new Uint8Array(bin.length); for (var i = 0; i < bin.length; i++) u[i] = bin.charCodeAt(i); return u.buffer; }
	window.FarfieldEditorAssets = { wasm: b(%q), fonts: [%s] };
})();</script>`, base64.StdEncoding.EncodeToString(bin), strings.Join(fonts, ", "))
	}
	var buf bytes.Buffer
	err := gallery.Execute(&buf, map[string]string{
		"Fonts":   theme.Fonts,
		"UI":      theme.Styles,
		"HostCSS": editor.HostCSS,
		"HostJS":  editor.HostJS,
		"MountJS": editor.MountJS,
		"Assets":  assets,
		"Sample":  editor.PreviewSample,
	})
	return buf.Bytes(), err
}

// stripSkeleton drops the doctype, <html>, <head>, <meta> and <body> tags,
// for hosts (an artifact viewer) that wrap the page in their own document.
func stripSkeleton(page []byte) []byte {
	s := string(page)
	for _, t := range []string{
		"<!doctype html>\n", "<html lang=\"en\">\n", "<head>\n", "<meta charset=\"utf-8\">\n",
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1, viewport-fit=cover\">\n",
		"</head>\n", "<body>\n", "</body>\n", "</html>\n",
	} {
		s = strings.Replace(s, t, "", 1)
	}
	return []byte(s)
}
