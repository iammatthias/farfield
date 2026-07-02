// Command keys is the farfield credential admin — the one place API keys for
// every app are issued, scoped, and revoked. It writes the shared key store
// (keys.sqlite on the /data volume) that the other apps read through
// lib/keys; a key minted here works within a request, a key revoked here
// stops within a request, no restarts. Env keys (<APP>_API_KEY and friends)
// remain untouched as the bootstrap/break-glass tier.
package main

import (
	"log/slog"
	"os"

	"github.com/iammatthias/farfield/lib/store"
	"github.com/iammatthias/farfield/lib/web"
)

func main() {
	_ = store.LoadEnv() // finds the root .env, wherever the app is run from
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// "health" probes the running server's /status for Docker healthchecks.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(web.Health(store.Env("KEYS_PORT", "8801")))
	}

	host := store.Env("HOST", "127.0.0.1")
	port := store.Env("KEYS_PORT", "8801")

	if err := run(host, port); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
