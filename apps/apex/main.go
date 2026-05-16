// Command apex serves the Farfield apex site — farfield.systems, the public
// face of the project.
//
// The site's assets are embedded into the binary, so it is self-contained:
// no volume, no external files. It will grow over time; for now it is a
// single page.
package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
)

//go:embed web
var webFS embed.FS

func main() {
	addr := envOr("FARFIELD_APEX_ADDR", "127.0.0.1:8790")
	h, err := handler()
	if err != nil {
		log.Fatalf("loading embedded site: %v", err)
	}
	log.Printf("farfield-apex listening on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, h))
}

// handler serves the embedded web/ directory; "/" resolves to index.html.
func handler() (http.Handler, error) {
	site, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}
	return http.FileServer(http.FS(site)), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
