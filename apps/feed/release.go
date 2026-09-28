package main

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// blobRefRe finds the blob CIDs a post body embeds.
var blobRefRe = regexp.MustCompile(`blob://(b[a-z2-7]{20,})`)

func blobCIDs(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range blobRefRe.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// releaseMedia asks blobs to delete each CID unless something still embeds it.
//
// feed owns the upload of a post's photos, so it owns letting them go: when a
// post is taken back (/undo) or never finished being made (an upload that
// failed partway), its photos would otherwise stay public at their CID with
// nothing pointing at them. blobs does the reference check — a deduped photo
// can live in another post or an essay — and refuses when it cannot see every
// source, so this can only ever remove a true orphan.
//
// Best effort and in the background: the post is already gone (or was never
// made), and a photo that could not be released is left for the hygiene page.
func (s *Server) releaseMedia(cids []string) {
	if len(cids) == 0 || s.blobsURL == "" {
		return
	}
	go func() {
		for _, cid := range cids {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
				strings.TrimRight(s.blobsURL, "/")+"/blobs/"+cid+"?unlessReferenced=1", nil)
			if err != nil {
				cancel()
				continue
			}
			req.Header.Set("X-API-Key", s.blobsKey)
			resp, err := embedClient.Do(req)
			cancel()
			if err != nil {
				slog.Warn("release media", "cid", cid, "err", err)
				continue
			}
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				slog.Info("released media", "cid", cid)
			case http.StatusConflict:
				slog.Info("media still referenced elsewhere; kept", "cid", cid)
			default:
				slog.Warn("release media", "cid", cid, "status", resp.StatusCode)
			}
		}
	}()
}
