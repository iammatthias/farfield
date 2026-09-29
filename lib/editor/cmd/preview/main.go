// Command preview serves the editor on its own page, or writes that page as a
// single self-contained HTML file, so the editor can be tried before any app
// adopts it.
//
//	go run ./lib/editor/cmd/preview                 serve on localhost:7070
//	go run ./lib/editor/cmd/preview -out page.html  write one file
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/iammatthias/farfield/lib/editor"
)

func main() {
	addr := flag.String("addr", "localhost:7070", "listen address")
	out := flag.String("out", "", "write a self-contained preview page here and exit")
	fragment := flag.Bool("artifact", false, "with -out: omit the document skeleton (for hosts that add their own)")
	flag.Parse()

	if *out != "" {
		page, err := editor.PreviewHTML(true, "")
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
		page, err := editor.PreviewHTML(false, "/static/editor/")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	log.Printf("editor preview on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// stripSkeleton drops the doctype, <html>, <head>, <meta> and <body> tags,
// for hosts (an artifact viewer) that wrap the page in their own document.
func stripSkeleton(page []byte) []byte {
	s := string(page)
	for _, t := range []string{
		"<!doctype html>\n", "<html lang=\"en\">\n", "<head>\n", "<meta charset=\"utf-8\">\n",
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n",
		"</head>\n", "<body>\n", "</body>\n", "</html>\n",
	} {
		s = strings.Replace(s, t, "", 1)
	}
	return []byte(s)
}
