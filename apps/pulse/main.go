// Command pulse is the farfield uptime-monitoring and traffic-analytics
// service. A checker loop probes configured HTTP targets on their own
// cadences and records checks and incidents; a collector loop rolls up the
// privacy-preserving request logs that sibling apps write via lib/pulse into
// daily aggregates. One session-gated console shows both. The only public
// surface is GET /status, which exposes nothing beyond a target count.
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
		port := store.Env("PULSE_PORT", "8798")
		if err := run(host, port); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "health":
		// Probes /status — backs the Docker healthcheck (distroless: no
		// curl). The checker and collector loops only ever start under
		// `serve`, so health mode runs none of them.
		os.Exit(web.Health(store.Env("PULSE_PORT", "8798")))
	default:
		fmt.Fprintln(os.Stderr, "usage: pulse [serve | health]")
		os.Exit(2)
	}
}
