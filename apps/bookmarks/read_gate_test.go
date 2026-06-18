package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBookmarksReadGate(t *testing.T) {
	s := newTestServer(t)
	s.auth.ReadKey = "rk"
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

	for _, path := range []string{"/api/bookmarks", "/api/bookmarks/1", "/api/categories"} {
		if got := code(path, ""); got != http.StatusUnauthorized {
			t.Errorf("%s no token = %d, want 401", path, got)
		}
	}
	if got := code("/api/bookmarks", "rk"); got != http.StatusOK {
		t.Errorf("/api/bookmarks read key = %d, want 200", got)
	}
	if got := code("/api/bookmarks", "k1"); got != http.StatusOK {
		t.Errorf("/api/bookmarks write key = %d, want 200", got)
	}
	if got := code("/status", ""); got != http.StatusOK {
		t.Errorf("/status no token = %d, want 200 (public)", got)
	}
}
