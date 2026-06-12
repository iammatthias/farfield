// Command scrap is the farfield personal pastebin. The author (browser
// session or API key) creates text pastes; anyone with the link views them,
// server-rendered with syntax highlighting. Pastes are content-addressed —
// the id is the CID of the body, so identical bodies collapse to one row.
// Visibility is public (listed), unlisted (link-only), or private (session);
// an optional per-paste view token gates all reads. Expiry is enforced lazily
// on read plus an hourly sweep. The compose/manage UI is session-gated by the
// shared PASSWORD; the raw-text terminal API is gated by SCRAP_API_KEY.
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
		port := store.Env("SCRAP_PORT", "8799")
		if err := run(host, port); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "health":
		// Probes /status — backs the Docker healthcheck (distroless: no curl).
		os.Exit(web.Health(store.Env("SCRAP_PORT", "8799")))
	default:
		fmt.Fprintln(os.Stderr, "usage: scrap [serve | health]")
		os.Exit(2)
	}
}
