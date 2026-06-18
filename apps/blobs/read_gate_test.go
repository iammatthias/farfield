package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/iammatthias/farfield/lib/web"
)

// TestBlobsReadGate confirms the index LIST is bearer-gated while the per-CID
// byte route is not — image bytes are loaded by browsers that cannot send a
// token, so they must stay public.
func TestBlobsReadGate(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "blobs.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{db: db, auth: &web.Auth{DB: db, APIKey: "write", ReadKey: "read"}}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	code := func(path, token string) int {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// The enumerating list is gated.
	if got := code("/blobs", ""); got != http.StatusUnauthorized {
		t.Errorf("GET /blobs no token = %d, want 401", got)
	}
	if got := code("/blobs", "read"); got != http.StatusOK {
		t.Errorf("GET /blobs read key = %d, want 200", got)
	}
	if got := code("/blobs", "write"); got != http.StatusOK {
		t.Errorf("GET /blobs write key = %d, want 200", got)
	}
	// The per-CID byte route is NOT behind the read gate: an unknown CID is a
	// 404 (not a 401), proving the gate does not cover it.
	if got := code("/blobs/baaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ""); got == http.StatusUnauthorized {
		t.Errorf("GET /blobs/{cid} must stay public, got 401")
	}
}
