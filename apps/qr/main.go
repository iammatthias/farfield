// Command qr is the farfield QR-code service. It encodes admin-supplied
// payloads as QR codes — either directly (the QR carries the exact payload)
// or via an editable proxy (the QR carries a stable public URL on this
// service that redirects to the current target, so the target can be edited
// later without reprinting the QR). Records have stable short IDs and a
// content CID; public/enabled controls gate the public image and redirect
// endpoints. The HTML admin lives at / and is session-gated by the shared
// PASSWORD; the JSON write API is gated by QR_API_KEY when set.
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
	port := store.Env("QR_PORT", "8794")

	if err := run(host, port); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
