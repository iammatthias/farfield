package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/iammatthias/farfield/lib/web"
)

func TestFeedReadGate(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "feed.sqlite"))
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

	if got := code("/api/posts", ""); got != http.StatusUnauthorized {
		t.Errorf("/api/posts no token = %d, want 401", got)
	}
	if got := code("/api/posts", "read"); got != http.StatusOK {
		t.Errorf("/api/posts read key = %d, want 200", got)
	}
	if got := code("/api/posts", "write"); got != http.StatusOK {
		t.Errorf("/api/posts write key = %d, want 200", got)
	}
	// A single post by slug is public (the "view source" endpoint), so an
	// anonymous read reaches the handler: a missing slug 404s rather than 401s.
	if got := code("/api/posts/anything", ""); got != http.StatusNotFound {
		t.Errorf("/api/posts/{slug} no token = %d, want 404 (public)", got)
	}
	if got := code("/status", ""); got != http.StatusOK {
		t.Errorf("/status no token = %d, want 200 (public)", got)
	}
}
