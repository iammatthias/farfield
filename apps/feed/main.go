// Command feed is the Farfield feed service (feed.farfield.systems).
//
// Identical in behaviour to the content service — the shared records engine —
// serving the feed collection from its own SQLite database. RSS and rendering
// are the website's job, not the backend's.
package main

import (
	"log"
	"os"

	"github.com/iammatthias/farfield/lib/records"
)

func main() {
	log.Fatal(records.Serve(records.Config{
		Addr:        envOr("FARFIELD_FEED_ADDR", "127.0.0.1:8788"),
		DBPath:      envOr("FARFIELD_FEED_DB", "farfield-feed.db"),
		SchemaDir:   envOr("FARFIELD_FEED_SCHEMAS", "schemas/feed"),
		Tokens:      records.TokensFromEnv(),
		ServiceName: "feed",
	}))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
