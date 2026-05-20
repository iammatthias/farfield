// Command bookmarks is the farfield bookmarks service — a curated list of
// links. Each bookmark carries its URL, a category, an admin-controlled
// public/private flag, and fetched OpenGraph metadata. It serves an HTML
// admin UI (under /admin), a public index of public bookmarks at /, and a
// JSON read API that hides private records and admin-only notes.
package main

import (
	"log/slog"
	"os"

	"github.com/iammatthias/farfield/lib/store"
)

func main() {
	_ = store.LoadEnv() // finds the root .env, wherever the app is run from
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	host := store.Env("HOST", "127.0.0.1")
	port := store.Env("BOOKMARKS_PORT", "8793")

	if err := run(host, port); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
