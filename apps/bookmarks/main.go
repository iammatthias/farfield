// Command bookmarks is the farfield bookmarks service — a curated list of
// links. Each bookmark carries its URL, a category, an admin-controlled
// public/private flag, and fetched OpenGraph metadata. It serves a
// session-gated CRUD UI at / and a JSON read API that hides private records
// and admin-only notes.
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
		port := store.Env("BOOKMARKS_PORT", "8793")
		if err := run(host, port); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "health":
		// Probes /status — backs the Docker healthcheck (distroless: no curl).
		os.Exit(web.Health(store.Env("BOOKMARKS_PORT", "8793")))
	default:
		fmt.Fprintln(os.Stderr, "usage: bookmarks [serve | health]")
		os.Exit(2)
	}
}
