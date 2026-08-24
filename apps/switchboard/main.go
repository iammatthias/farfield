// Command switchboard is the farfield message router — the fleet's front door
// for iMessage. Photon delivers an inbound iMessage to a signed webhook here;
// switchboard authenticates it, works out what the message meant, and dispatches
// it to the app that owns that job: a thought becomes a feed post, a bare link
// becomes a bookmark, /scrap becomes a paste, /qr becomes a QR code. It replies
// in the same thread with what happened.
//
// It holds the service API keys so the phone never has to. It deliberately does
// not talk to blobs: apps own their own media, so a texted photo goes to feed,
// which does the blob upload itself.
//
// Usage:
//
//	switchboard                    serve the HTTP service (default)
//	switchboard health             probe /status (backs the Docker healthcheck)
//	switchboard probe              dump recent messages from the line as JSON
//	switchboard probe <guid>       download one attachment, report its size
//	switchboard probe upload       upload a test image, report the guid
//	switchboard probe send <file>  upload and send a file, as separate steps
//
// The probe subcommands exist because the webhook is otherwise the only view of
// a message, and this integration's real behaviour has differed from its
// documentation in several places — content shape, attachment field names, and
// which requests need a chatGuid to route. They ask the line directly, so a
// message that already arrived can be inspected without sending another.
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

	if len(os.Args) > 1 && os.Args[1] == "probe" {
		run := func() error {
			if len(os.Args) > 3 && os.Args[2] == "send" {
				return runProbeSend(os.Args[3])
			}
			if len(os.Args) > 2 && os.Args[2] == "upload" {
				return runProbeUpload()
			}
			if len(os.Args) > 2 {
				return runProbeDownload(os.Args[2])
			}
			return runProbe(10)
		}
		if err := run(); err != nil {
			slog.Error("probe", "err", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "health" {
		// Probes /status — backs the Docker healthcheck (distroless: no curl).
		os.Exit(web.Health(store.Env("SWITCHBOARD_PORT", "8802")))
	}

	host := store.Env("HOST", "127.0.0.1")
	port := store.Env("SWITCHBOARD_PORT", "8802")

	if err := run(host, port); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
