package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/iammatthias/farfield/lib/store"
)

// runProbe dumps recent messages from the line as JSON.
//
// It exists because the webhook is the only view switchboard normally has of a
// message, and when a delivery flattens to nothing there is no way to tell
// which arm of the content union it used. This asks the line directly, so a
// message that already arrived can be inspected without asking anyone to send
// another one.
//
// Output is the server's own protobuf rendered as JSON — field names here are
// the wire contract, so what it prints is exactly what the parser has to match.
func runProbe(n int) error {
	client, err := newPhotonClient(
		store.Env("SPECTRUM_PROJECT_ID", ""),
		store.Env("SPECTRUM_PROJECT_SECRET", ""),
		store.Env("SPECTRUM_IMESSAGE_ADDRESS", "imessage.spectrum.photon.codes:443"),
		store.Env("SPECTRUM_CLOUD_URL", "https://spectrum.photon.codes"),
	)
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("set SPECTRUM_PROJECT_ID and SPECTRUM_PROJECT_SECRET")
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgs, err := client.RecentMessages(ctx, dmChatGUID(store.Env("SWITCHBOARD_ALLOW", "")), n)
	if err != nil {
		return err
	}
	marshal := protojson.MarshalOptions{Multiline: true, Indent: "  ", EmitUnpopulated: false}
	for _, m := range msgs {
		out, err := marshal.Marshal(m)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, string(out))
	}
	fmt.Fprintf(os.Stderr, "%d messages\n", len(msgs))
	return nil
}

// runProbeDownload pulls one attachment by guid and reports what came back.
// It answers the question the webhook cannot: whether the bytes are reachable
// and intact, independent of whether the parser recognized the message.
func runProbeDownload(guid string) error {
	client, err := newPhotonClient(
		store.Env("SPECTRUM_PROJECT_ID", ""),
		store.Env("SPECTRUM_PROJECT_SECRET", ""),
		store.Env("SPECTRUM_IMESSAGE_ADDRESS", "imessage.spectrum.photon.codes:443"),
		store.Env("SPECTRUM_CLOUD_URL", "https://spectrum.photon.codes"),
	)
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("set SPECTRUM_PROJECT_ID and SPECTRUM_PROJECT_SECRET")
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	data, err := client.Download(ctx, guid)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "%d bytes\n", len(data))
	return nil
}

// runProbeUpload tries to put a small file on the line. Sending an image
// requires an upload first — AttachmentRef only ever references a guid — so
// whether this works decides whether image replies are possible at all.
func runProbeUpload() error {
	client, err := newPhotonClient(
		store.Env("SPECTRUM_PROJECT_ID", ""),
		store.Env("SPECTRUM_PROJECT_SECRET", ""),
		store.Env("SPECTRUM_IMESSAGE_ADDRESS", "imessage.spectrum.photon.codes:443"),
		store.Env("SPECTRUM_CLOUD_URL", "https://spectrum.photon.codes"),
	)
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A 1x1 PNG — the smallest thing that is unambiguously a real image.
	png := []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
		0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
		0, 0, 0, 0x0a, 'I', 'D', 'A', 'T', 0x78, 0x9c, 0x63, 0, 1, 0, 0, 5, 0, 1,
		0x0d, 0x0a, 0x2d, 0xb4, 0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
	}
	guid, err := client.upload(ctx, "probe.png", png)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "uploaded guid=%s\n", guid)
	return nil
}

// runProbeSend uploads a file and sends it, reporting which step fails.
// Upload and send are separate RPCs with separate failure modes, and the
// service only ever saw the combined error.
func runProbeSend(path string) error {
	client, err := newPhotonClient(
		store.Env("SPECTRUM_PROJECT_ID", ""),
		store.Env("SPECTRUM_PROJECT_SECRET", ""),
		store.Env("SPECTRUM_IMESSAGE_ADDRESS", "imessage.spectrum.photon.codes:443"),
		store.Env("SPECTRUM_CLOUD_URL", "https://spectrum.photon.codes"),
	)
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	chat := dmChatGUID(store.Env("SWITCHBOARD_ALLOW", ""))
	guid, err := client.upload(ctx, "probe.png", data)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	fmt.Fprintf(os.Stderr, "uploaded %s\n", guid)
	if err := client.sendAttachment(ctx, chat, guid); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	fmt.Fprintln(os.Stdout, "sent")
	return nil
}
