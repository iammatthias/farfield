// Command feed is the farfield feed service — a single stream of ephemeral,
// short-form posts ("thoughts"). It is content's stripped-down sibling: no
// collections, no titles — just dated markdown notes with an optional link
// and tags. It serves an HTML admin UI and a public JSON read API.
//
// Usage:
//
//	feed                          serve the HTTP service (default)
//	feed import-vault <dir>        import a directory of feed .md files
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

func main() {
	_ = store.LoadEnv() // finds the root .env, wherever the app is run from
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "serve":
		host := store.Env("HOST", "127.0.0.1")
		port := store.Env("FEED_PORT", "8788")
		if err := run(host, port); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "health":
		// Probes /status — backs the Docker healthcheck (distroless: no curl).
		os.Exit(web.Health(store.Env("FEED_PORT", "8788")))
	case "import-vault":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: feed import-vault <dir>")
			os.Exit(2)
		}
		if err := runImportVault(os.Args[2]); err != nil {
			slog.Error("import-vault failed", "err", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: feed [serve | health | import-vault <dir>]")
		os.Exit(2)
	}
}

// runImportVault opens the database and imports a directory of feed files.
func runImportVault(dir string) error {
	db, err := openDB(store.Env("FEED_DB_PATH", "feed.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	return importVault(db, dir)
}
