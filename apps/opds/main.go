// Command opds is the farfield e-book service — an OPDS catalog of EPUBs. It
// exposes an HTML admin UI for uploading and moderating books, an OPDS
// acquisition feed that e-reader apps browse over HTTP Basic Auth, and an
// API-key-gated upload API. Book bytes (and cover images) live in a byte store
// — a local directory or Cloudflare R2 — while metadata lives in SQLite.
//
// Usage:
//
//	opds   serve the HTTP service (default and only mode)
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
	port := store.Env("OPDS_PORT", "8797")
	if err := run(host, port); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
