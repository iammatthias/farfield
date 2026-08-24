package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/iammatthias/farfield/lib/web"
)

// maxMediaPost caps one multipart post. It matches the blobs service's own
// limit, so this endpoint never accepts more than blobs would store.
const maxMediaPost = web.MaxEmbedUpload

// maxMediaField caps a text field inside the multipart body — far above any
// real post, well below abuse.
const maxMediaField = 1 << 20

// handleAPICreateMedia creates a post from a multipart upload: text fields plus
// any number of image parts.
//
// It exists so callers with bytes in hand — switchboard, relaying a photo
// texted over iMessage — do not need a blobs key of their own. feed already
// holds one because it owns the posts that reference blobs, so the upload
// belongs on this side of the boundary: one service owns media, and everyone
// else hands it the bytes.
//
// Parts stream straight through to blobs rather than being buffered, the same
// way web.ProxyUpload works, so a large photo never lands in memory here.
func (s *Server) handleAPICreateMedia(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMediaPost)
	mr, err := r.MultipartReader()
	if err != nil {
		web.WriteError(w, http.StatusBadRequest, "expected a multipart body")
		return
	}

	var (
		body string
		tags []string
		cids []string
	)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			web.WriteError(w, http.StatusBadRequest, "invalid upload")
			return
		}
		switch part.FormName() {
		case "body":
			value, err := io.ReadAll(io.LimitReader(part, maxMediaField))
			if err != nil {
				web.WriteError(w, http.StatusBadRequest, "invalid body field")
				return
			}
			body = strings.TrimSpace(string(value))
		case "tags":
			value, err := io.ReadAll(io.LimitReader(part, maxMediaField))
			if err != nil {
				web.WriteError(w, http.StatusBadRequest, "invalid tags field")
				return
			}
			tags = splitTags(string(value))
		case "file":
			cid, err := s.storeBlob(r, part)
			if err != nil {
				slog.Error("store media", "err", err)
				web.WriteError(w, http.StatusBadGateway, "could not store media")
				return
			}
			cids = append(cids, cid)
		}
		part.Close()
	}

	if body == "" && len(cids) == 0 {
		web.WriteError(w, http.StatusBadRequest, "a post needs a body or an image")
		return
	}

	p := &Post{Body: composeMediaBody(body, cids), Tags: tags}
	if err := insertPost(s.db, p); err != nil {
		web.WriteError(w, http.StatusInternalServerError, "could not create post")
		return
	}
	web.WriteJSON(w, http.StatusCreated, p)
}

// composeMediaBody puts the images under the text, one embed per line — the
// shape the feed renderer already understands.
//
// Deliberately not a content series: feed's renderer is built with no series
// resolver, so a series:// ref would render as literal text in feed's own UI.
// Plain blob:// embeds render everywhere.
func composeMediaBody(body string, cids []string) string {
	if len(cids) == 0 {
		return body
	}
	embeds := make([]string, 0, len(cids))
	for _, cid := range cids {
		embeds = append(embeds, "![](blob://"+cid+")")
	}
	joined := strings.Join(embeds, "\n\n")
	if body == "" {
		return joined
	}
	return body + "\n\n" + joined
}

// storeBlob streams one multipart part into the blobs service and returns its
// content id. blobs derives the MIME type from the bytes themselves, so the
// part's declared type is passed along as a hint but nothing depends on it.
func (s *Server) storeBlob(r *http.Request, part *multipart.Part) (string, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimRight(s.blobsURL, "/")+"/blobs", part)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", s.blobsKey)
	if ct := part.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := embedClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return "", fmt.Errorf("blobs %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	var meta struct {
		CID string `json:"cid"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&meta); err != nil {
		return "", err
	}
	if meta.CID == "" {
		return "", fmt.Errorf("blobs returned no cid")
	}
	return meta.CID, nil
}
