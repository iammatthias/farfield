// Command calendar is the farfield calendar service — a daily photo calendar.
// Each day shows NASA's Astronomy Picture of the Day. It exposes HTML pages for
// browsing and a public JSON API the website can read. Photos are cached in
// SQLite, so APOD is touched only on a cache miss and an upstream outage never
// breaks the calendar.
//
// Usage:
//
//	calendar    serve the HTTP service
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

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backfill":
			start, end := "2026-01-01", todayUTC()
			if len(os.Args) > 2 {
				start = os.Args[2]
			}
			if len(os.Args) > 3 {
				end = os.Args[3]
			}
			if err := backfillCommand(start, end); err != nil {
				slog.Error("backfill failed", "err", err)
				os.Exit(1)
			}
			return
		case "health":
			// Probes /status — backs the Docker healthcheck (distroless: no curl).
			os.Exit(web.Health(store.Env("CALENDAR_PORT", "8792")))
		case "serve":
			// explicit serve is accepted; falling through starts the HTTP service
		default:
			_, _ = fmt.Fprintf(os.Stderr, "usage: calendar [serve|health|backfill [start YYYY-MM-DD] [end YYYY-MM-DD]]\n")
			os.Exit(2)
		}
	}

	host := store.Env("HOST", "127.0.0.1")
	port := store.Env("CALENDAR_PORT", "8792")
	if err := run(host, port); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
