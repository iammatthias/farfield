// Command library is the farfield e-book service — an OPDS catalog of EPUBs. It
// exposes an HTML admin UI for uploading and moderating books, an OPDS
// acquisition feed that e-reader apps browse over HTTP Basic Auth, and an
// API-key-gated upload API. Book bytes (and cover images) live in a byte store
// — a local directory or Cloudflare R2 — while metadata lives in SQLite.
//
// Usage:
//
//	library [serve]   serve the HTTP service (the default)
//	library health    probe the running service's /status endpoint
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
		port := store.Env("LIBRARY_PORT", "8797")
		if err := run(host, port); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "health":
		// Probes /status — backs the Docker healthcheck (distroless: no curl).
		os.Exit(web.Health(store.Env("LIBRARY_PORT", "8797")))
	default:
		fmt.Fprintln(os.Stderr, "usage: library [serve | health]")
		os.Exit(2)
	}
}
