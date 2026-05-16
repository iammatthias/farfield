package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// cmdUploadMedia uploads local media files — images, video, PDFs, anything —
// to the blob service, creates a media record for each, and prints the blob
// CID, one per line. It is the authoring counterpart to migrate-images: that
// lifts ipfs:// images already in records; this registers a new local file so
// a body can reference it as blob://<cid>. The Obsidian plugin shells out to
// this on paste.
func cmdUploadMedia(args []string) error {
	fs := flag.NewFlagSet("upload-media", flag.ExitOnError)
	blobs := fs.String("blobs", defaultBlobs, "blob service URL")
	content := fs.String("content", defaultService, "content service URL")
	_ = fs.Parse(args)
	files, trailing := splitPositionals(fs.Args())
	_ = fs.Parse(trailing)
	if len(files) == 0 {
		return fmt.Errorf("usage: farfield upload-media <file>... [flags]")
	}

	tok := token()
	var failed int
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fail  %s — %v\n", f, err)
			failed++
			continue
		}
		cid, _, err := storeMedia(*blobs, *content, tok, data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fail  %s — %v\n", f, err)
			failed++
			continue
		}
		// The CID is the only thing on stdout — callers (the plugin) parse it.
		fmt.Println(cid)
	}
	if failed > 0 {
		return fmt.Errorf("%d failure(s)", failed)
	}
	return nil
}

// storeMedia uploads bytes to the blob service, which derives and stores the
// metadata, then creates the matching media record in the content service. It
// returns the blob CID and the derived metadata. Shared by upload-media and
// migrate-images. The blob service accepts any media type — images get
// dimensions and a blurhash, everything else just a size and MIME type.
func storeMedia(blobs, content, tok string, data []byte) (string, map[string]any, error) {
	upReq, err := http.NewRequest(http.MethodPost, blobs+"/blobs", bytes.NewReader(data))
	if err != nil {
		return "", nil, err
	}
	upReq.Header.Set("Authorization", "Bearer "+tok)
	upResp, err := httpClient.Do(upReq)
	if err != nil {
		return "", nil, fmt.Errorf("blob upload: %w", err)
	}
	defer upResp.Body.Close()
	if upResp.StatusCode >= 300 {
		body, _ := io.ReadAll(upResp.Body)
		return "", nil, fmt.Errorf("blob service returned %d: %s", upResp.StatusCode, body)
	}
	var meta map[string]any
	if err := json.NewDecoder(upResp.Body).Decode(&meta); err != nil {
		return "", nil, fmt.Errorf("parsing blob response: %w", err)
	}
	cid, _ := meta["cid"].(string)
	if cid == "" {
		return "", nil, fmt.Errorf("blob response had no cid")
	}

	// The blob service's metadata fields line up with the media schema; stamp
	// the timestamp and store it as the media record (rkey = CID).
	meta["created"] = time.Now().UTC().Format(time.RFC3339)
	if _, err := send(content, "media", cid, meta, tok); err != nil {
		return "", nil, fmt.Errorf("media record: %w", err)
	}
	return cid, meta, nil
}
