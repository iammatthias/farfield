// Command blobs is the Farfield blob service (blobs.farfield.systems).
//
// A standalone content-addressed image store. The content app (and any
// future app) upload images here and reference them by CID. v1 uses the
// local-directory backend so the whole stack runs on one machine; an R2
// backend slots in behind the blob.Store interface for the server.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/iammatthias/farfield/lib/blob"
	"github.com/iammatthias/farfield/lib/httpkit"
)

// maxUpload caps an upload at 50 MiB, enforced on bytes actually read.
const maxUpload = 50 << 20

type service struct {
	store  blob.Store
	tokens []string
}

func main() {
	addr := envOr("FARFIELD_BLOBS_ADDR", "127.0.0.1:8789")

	store, desc, err := openStore()
	if err != nil {
		log.Fatalf("opening blob store: %v", err)
	}
	svc := &service{store: store, tokens: tokensFromEnv()}
	log.Printf("blob backend: %s", desc)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", svc.root)
	mux.HandleFunc("GET /status", svc.status)
	mux.HandleFunc("GET /blobs", svc.list)
	mux.HandleFunc("POST /blobs", svc.upload)
	mux.HandleFunc("GET /blobs/{cid}", svc.getBlob)
	mux.HandleFunc("GET /blobs/{cid}/meta", svc.getMeta)

	log.Printf("farfield-blobs listening on http://%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// openStore picks the blob backend. FARFIELD_BLOBS_BACKEND is "local"
// (default) or "r2"; the R2 backend reads its credentials from R2_* env vars.
func openStore() (blob.Store, string, error) {
	switch envOr("FARFIELD_BLOBS_BACKEND", "local") {
	case "local":
		dir := envOr("FARFIELD_BLOBS_DIR", "farfield-blobs-data")
		s, err := blob.OpenLocalDir(dir)
		return s, "local:" + dir, err
	case "r2":
		bucket := os.Getenv("R2_BUCKET")
		s, err := blob.NewR2(blob.R2Config{
			AccountID:       os.Getenv("R2_ACCOUNT_ID"),
			AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
			Bucket:          bucket,
		})
		return s, "r2:" + bucket, err
	default:
		return nil, "", fmt.Errorf("unknown FARFIELD_BLOBS_BACKEND (want local|r2)")
	}
}

func (s *service) root(w http.ResponseWriter, _ *http.Request) {
	httpkit.WriteJSON(w, 200, map[string]any{
		"service": "farfield-blobs",
		"ok":      true,
		"endpoints": []string{
			"GET    /status",
			"GET    /blobs",
			"GET    /blobs/{cid}",
			"GET    /blobs/{cid}/meta",
			"POST   /blobs            (auth)",
		},
	})
}

func (s *service) status(w http.ResponseWriter, _ *http.Request) {
	cids, err := s.store.List()
	if err != nil {
		httpkit.WriteError(w, httpkit.Internal(err.Error()))
		return
	}
	httpkit.WriteJSON(w, 200, map[string]any{"service": "farfield-blobs", "ok": true, "blobs": len(cids)})
}

func (s *service) list(w http.ResponseWriter, _ *http.Request) {
	cids, err := s.store.List()
	if err != nil {
		httpkit.WriteError(w, httpkit.Internal(err.Error()))
		return
	}
	if cids == nil {
		cids = []string{}
	}
	httpkit.WriteJSON(w, 200, map[string]any{"blobs": cids})
}

func (s *service) upload(w http.ResponseWriter, r *http.Request) {
	if e := httpkit.VerifyBearer(r, s.tokens); e != nil {
		httpkit.WriteError(w, e)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		httpkit.WriteError(w, &httpkit.APIError{
			Status: http.StatusRequestEntityTooLarge, Code: "blob_too_large",
			Message: "upload exceeds the 50 MiB cap",
		})
		return
	}
	if len(data) == 0 {
		httpkit.WriteError(w, httpkit.BadRequest("empty_blob", "no bytes uploaded"))
		return
	}
	meta, err := blob.DeriveMetadata(data)
	if err != nil {
		httpkit.WriteError(w, httpkit.BadRequest("invalid_image", err.Error()))
		return
	}
	if err := s.store.Put(meta, data); err != nil {
		httpkit.WriteError(w, httpkit.Internal(err.Error()))
		return
	}
	httpkit.WriteJSON(w, 200, meta)
}

func (s *service) getBlob(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		httpkit.WriteError(w, httpkit.BadRequest("invalid_cid", "malformed CID"))
		return
	}
	data, err := s.store.GetBytes(cid)
	if err != nil {
		httpkit.WriteError(w, httpkit.Internal(err.Error()))
		return
	}
	if data == nil {
		httpkit.WriteError(w, httpkit.NotFound("blob "+cid))
		return
	}
	mime := "application/octet-stream"
	if m, _ := s.store.GetMeta(cid); m != nil {
		mime = m.Mime
	}
	w.Header().Set("Content-Type", mime)
	// Content-addressed: the bytes for a CID never change.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

func (s *service) getMeta(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !validCID(cid) {
		httpkit.WriteError(w, httpkit.BadRequest("invalid_cid", "malformed CID"))
		return
	}
	meta, err := s.store.GetMeta(cid)
	if err != nil {
		httpkit.WriteError(w, httpkit.Internal(err.Error()))
		return
	}
	if meta == nil {
		httpkit.WriteError(w, httpkit.NotFound("blob "+cid))
		return
	}
	httpkit.WriteJSON(w, 200, meta)
}

// validCID accepts only the base32 CIDv1 alphabet, so a CID path segment can
// never be a path-traversal payload.
func validCID(cid string) bool {
	if len(cid) < 1 || len(cid) > 80 {
		return false
	}
	for _, c := range cid {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func tokensFromEnv() []string {
	var tokens []string
	for _, v := range []string{"FARFIELD_TOKEN", "FARFIELD_TOKEN_PREVIOUS"} {
		if t := os.Getenv(v); t != "" {
			tokens = append(tokens, t)
		}
	}
	if len(tokens) == 0 {
		log.Println("warning: FARFIELD_TOKEN unset — using 'dev-token' (local dev only)")
		tokens = []string{"dev-token"}
	}
	return tokens
}
