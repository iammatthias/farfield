// Command sideload is the farfield self-hosted ad-hoc .ipa installer. It takes
// a signed ad-hoc iOS build, content-addresses the .ipa, extracts its metadata
// (bundle id, version, provisioning-profile expiry, enrolled devices),
// generates the OTA manifest iOS expects, and serves a one-tap install page —
// so a registered iPhone installs the latest build from mobile Safari with no
// cable and no laptop in reach.
//
// The index, build pages, and management UI are session-gated by the shared
// PASSWORD; agent uploads are gated by SIDELOAD_API_KEY. The install payload
// (manifest, .ipa, icons) is fetched by the iOS install daemon, which carries
// no cookie, so it lives under high-entropy capability tokens at /i/{token}/*.
// The same token mechanism backs public single-use share links at /s/{token}.
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
		port := store.Env("SIDELOAD_PORT", "8800")
		if err := run(host, port); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "health":
		// Probes /status — backs the Docker healthcheck (distroless: no curl).
		os.Exit(web.Health(store.Env("SIDELOAD_PORT", "8800")))
	default:
		fmt.Fprintln(os.Stderr, "usage: sideload [serve | health]")
		os.Exit(2)
	}
}
