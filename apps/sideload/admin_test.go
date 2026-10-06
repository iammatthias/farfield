package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdminShares(t *testing.T) {
	s := testServer(t)
	if _, err := insertBuild(s.db, &Build{ID: "build0000000001", CID: "bafybuild", BundleID: "com.example.app",
		AppName: "Example", Version: "1.2", CreatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	live, err := createShare(s.db, "build0000000001", time.Hour, 1, "for Sam")
	if err != nil {
		t.Fatal(err)
	}
	self, err := selfToken(s.db, "build0000000001")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	do := func(method, path, key string, headers map[string]string) (int, []byte) {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	for _, tc := range []struct {
		name    string
		key     string
		headers map[string]string
		want    int
	}{
		{"tunnel", "apikey", map[string]string{"Cf-Ray": "x"}, 404},
		{"tunnel client ip", "apikey", map[string]string{"Cf-Connecting-Ip": "203.0.113.4"}, 404},
		{"wrong key", "nope", nil, 401},
		{"write key", "apikey", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := do("GET", "/api/admin/shares", tc.key, tc.headers); code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", code, tc.want, body)
			}
		})
	}

	_, body := do("GET", "/api/admin/shares", "apikey", nil)
	var list struct{ Shares []shareView }
	_ = json.Unmarshal(body, &list)
	if len(list.Shares) != 1 {
		t.Fatalf("shares = %s, want the one share (self tokens are not shares)", body)
	}
	got := list.Shares[0]
	if got.Token != live.Token || got.AppName != "Example" || got.Label != "for Sam" || got.MaxInstalls != 1 ||
		got.Installs != 0 || got.Revoked || !got.Live || got.ShareURL != "https://sideload.test/s/"+live.Token {
		t.Errorf("share = %+v", got)
	}

	code, body := do("POST", "/api/admin/shares/"+live.Token+"/revoke", "apikey", nil)
	var revoked shareView
	_ = json.Unmarshal(body, &revoked)
	if code != 200 || !revoked.Revoked || revoked.Live || revoked.State != stateRevoked {
		t.Errorf("revoke = %d %s", code, body)
	}
	if tok, _ := getToken(s.db, live.Token); tok.canStart() {
		t.Error("a revoked share can still start an install")
	}
	// Idempotent: revoking again reports the same state.
	if code, _ := do("POST", "/api/admin/shares/"+live.Token+"/revoke", "apikey", nil); code != 200 {
		t.Errorf("second revoke = %d, want 200", code)
	}
	if code, _ := do("POST", "/api/admin/shares/"+self.Token+"/revoke", "apikey", nil); code != 404 {
		t.Errorf("revoke self token = %d, want 404", code)
	}
	if tok, _ := getToken(s.db, self.Token); tok.State != stateActive {
		t.Error("the admin revoke touched a self token")
	}
	if code, _ := do("POST", "/api/admin/shares/nope/revoke", "apikey", nil); code != 404 {
		t.Errorf("revoke missing = %d, want 404", code)
	}
}
