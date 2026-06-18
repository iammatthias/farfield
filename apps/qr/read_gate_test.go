package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestQRReadGate confirms the code LIST/detail is bearer-gated while the public
// scan (/qr/{id}) and redirect (/r/{id}) routes stay open — strangers scan them.
func TestQRReadGate(t *testing.T) {
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

	if got := code("/api/codes", ""); got != http.StatusUnauthorized {
		t.Errorf("/api/codes no token = %d, want 401", got)
	}
	if got := code("/api/codes", "rk"); got != http.StatusOK {
		t.Errorf("/api/codes read key = %d, want 200", got)
	}
	if got := code("/api/codes", "k1"); got != http.StatusOK {
		t.Errorf("/api/codes write key = %d, want 200", got)
	}
	// The scan and redirect routes must not be gated — an unknown id is 404,
	// never 401.
	for _, path := range []string{"/qr/nonexistent", "/r/nonexistent"} {
		if got := code(path, ""); got == http.StatusUnauthorized {
			t.Errorf("%s must stay public, got 401", path)
		}
	}
}
