// Command content is the Farfield content service (content.farfield.systems).
//
// A thin binary over the shared records engine: it serves the posts,
// open-source, recipes, art, melange, media, and series collections from its
// own SQLite database. Configured by environment for local dev.
package main

import (
	"log"
	"os"

	"github.com/iammatthias/farfield/lib/records"
)

func main() {
	log.Fatal(records.Serve(records.Config{
		Addr:        envOr("FARFIELD_CONTENT_ADDR", "127.0.0.1:8787"),
		DBPath:      envOr("FARFIELD_CONTENT_DB", "farfield-content.db"),
		SchemaDir:   envOr("FARFIELD_CONTENT_SCHEMAS", "schemas/content"),
		Tokens:      records.TokensFromEnv(),
		ServiceName: "content",
	}))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
