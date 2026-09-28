package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
)

// A deleted blob is not gone while the edge still has it. Bytes are served
// with Cache-Control: public, max-age=31536000, immutable, so Cloudflare keeps
// a copy for up to a year after the origin starts answering 404 — which is
// exactly the wrong behaviour for a photo deleted because it should not have
// been public.
//
// purgeEdge asks Cloudflare to drop the cached URLs. It needs a token with
// Zone → Cache Purge on the blobs zone (CF_PURGE_TOKEN) and the zone id
// (CF_ZONE_ID); without them it is a logged no-op, since the delete itself
// has already succeeded and must not fail over the cache.
func (s *Server) purgeEdge(cids ...string) {
	token, zone := store.Env("CF_PURGE_TOKEN", ""), store.Env("CF_ZONE_ID", "")
	base := strings.TrimRight(store.Env("BLOBS_PUBLIC_URL", "https://blobs.farfield.systems"), "/")
	var files []string
	for _, c := range cids {
		if c != "" {
			files = append(files, base+"/blobs/"+c, base+"/blobs/"+c+"/meta")
		}
	}
	if len(files) == 0 {
		return
	}
	if token == "" || zone == "" {
		slog.Warn("blob deleted but the edge may still serve it — set CF_PURGE_TOKEN and CF_ZONE_ID to purge",
			"urls", len(files))
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := purgeCloudflare(ctx, cloudflareAPI, zone, token, files); err != nil {
			slog.Error("edge purge failed", "err", err, "urls", len(files))
			return
		}
		slog.Info("edge purged", "urls", len(files))
	}()
}

// cloudflareAPI is a variable so a test can point it at a stub.
var cloudflareAPI = "https://api.cloudflare.com/client/v4"

func purgeCloudflare(ctx context.Context, api, zone, token string, files []string) error {
	body, _ := json.Marshal(map[string]any{"files": files})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		api+"/zones/"+zone+"/purge_cache", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 || !out.Success {
		msg := resp.Status
		if len(out.Errors) > 0 {
			msg += ": " + out.Errors[0].Message
		}
		return &purgeError{msg}
	}
	return nil
}

type purgeError struct{ msg string }

func (e *purgeError) Error() string { return "cloudflare purge: " + e.msg }
