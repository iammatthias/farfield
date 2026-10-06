package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func scrapAdmin(t *testing.T, ts *httptest.Server, method, path, key string, headers map[string]string, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// seedPastes stores three pastes with fixed creation times, oldest first, the
// newest of them already expired and the middle one token-gated.
func seedPastes(t *testing.T, s *Server) (old, gated, expiredP *Paste) {
	t.Helper()
	mk := func(body, title, vis, token, created string) *Paste {
		p, err := s.createPaste(body, title, "go", vis, "never", token)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE pastes SET created_at = ? WHERE id = ?`, created, p.ID); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old = mk("old body", "old", VisPublic, "", "2026-01-01T00:00:00Z")
	gated = mk("gated body", "gated", VisUnlisted, "s3cret", "2026-01-02T00:00:00Z")
	expiredP = mk("expired body", "expired", VisPrivate, "", "2026-01-03T00:00:00Z")
	if _, err := s.db.Exec(`UPDATE pastes SET expires_at = '2026-01-04T00:00:00Z' WHERE id = ?`, expiredP.ID); err != nil {
		t.Fatal(err)
	}
	return old, gated, expiredP
}

func TestAdminPastesGate(t *testing.T) {
	_, ts := newTestServer(t)
	for _, tc := range []struct {
		name    string
		key     string
		headers map[string]string
		want    int
	}{
		{"tunnel", "k1", map[string]string{"Cf-Ray": "x"}, 404},
		{"tunnel client ip", "k1", map[string]string{"Cf-Connecting-Ip": "203.0.113.4"}, 404},
		{"no key", "", nil, 401},
		{"wrong key", "nope", nil, 401},
		{"write key", "k1", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if resp, body := scrapAdmin(t, ts, "GET", "/api/admin/pastes", tc.key, tc.headers, ""); resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
		})
	}
}

func TestAdminPastesList(t *testing.T) {
	s, ts := newTestServer(t)
	old, gated, expiredP := seedPastes(t, s)

	type listResp struct {
		Pastes []Paste
		Total  int
	}
	get := func(q string) listResp {
		resp, body := scrapAdmin(t, ts, "GET", "/api/admin/pastes"+q, "k1", nil, "")
		if resp.StatusCode != 200 {
			t.Fatalf("list%s = %d %s", q, resp.StatusCode, body)
		}
		var lr listResp
		_ = json.Unmarshal(body, &lr)
		return lr
	}
	all := get("")
	if all.Total != 3 || len(all.Pastes) != 3 {
		t.Fatalf("list = %+v, want all three (the expired one included)", all)
	}
	ids := []string{all.Pastes[0].ID, all.Pastes[1].ID, all.Pastes[2].ID}
	if ids[0] != expiredP.ID || ids[1] != gated.ID || ids[2] != old.ID {
		t.Errorf("order = %v, want newest first", ids)
	}
	for _, p := range all.Pastes {
		if p.Body != "" {
			t.Errorf("list carried a body for %s", p.ID)
		}
	}
	if all.Pastes[0].ExpiresAt == "" || !all.Pastes[1].HasToken {
		t.Errorf("expiresAt/hasToken missing: %+v", all.Pastes[:2])
	}
	if p2 := get("?limit=2&page=2"); p2.Total != 3 || len(p2.Pastes) != 1 || p2.Pastes[0].ID != old.ID {
		t.Errorf("page 2 of 2 = %+v", p2)
	}
	if p := get("?limit=0&page=-3"); len(p.Pastes) != 3 {
		t.Errorf("bad paging params should fall back to defaults, got %d", len(p.Pastes))
	}
	// Listing does not sweep.
	if p, _ := getPaste(s.db, expiredP.ID); p == nil {
		t.Error("listing deleted the expired paste")
	}
}

func TestAdminPasteGet(t *testing.T) {
	s, ts := newTestServer(t)
	_, gated, expiredP := seedPastes(t, s)

	for _, p := range []*Paste{gated, expiredP} {
		resp, body := scrapAdmin(t, ts, "GET", "/api/admin/pastes/"+p.ID, "k1", nil, "")
		var got Paste
		_ = json.Unmarshal(body, &got)
		if resp.StatusCode != 200 || got.Body != p.Body || resp.Header.Get("ETag") == "" {
			t.Errorf("get %s = %d %s", p.Title, resp.StatusCode, body)
		}
	}
	if p, _ := getPaste(s.db, gated.ID); p.Views != 0 {
		t.Errorf("admin read counted %d views", p.Views)
	}
	if p, _ := getPaste(s.db, expiredP.ID); p == nil {
		t.Error("admin read deleted the expired paste")
	}
	for _, id := range []string{"abcdefghijklmnop", "not-an-id"} {
		if resp, _ := scrapAdmin(t, ts, "GET", "/api/admin/pastes/"+id, "k1", nil, ""); resp.StatusCode != 404 {
			t.Errorf("get %s = %d, want 404", id, resp.StatusCode)
		}
	}
}

func TestAdminPasteUpdate(t *testing.T) {
	cases := []struct {
		name      string
		target    string // "old" (public, no token) or "gated"
		body      string
		want      int
		check     func(t *testing.T, p *Paste)
		unchanged bool
	}{
		{"title and lang", "old", `{"title":"  New  ","lang":" Rust "}`, 200, func(t *testing.T, p *Paste) {
			if p.Title != "New" || p.Lang != "rust" || p.Visibility != VisPublic || p.ExpiresAt != "" {
				t.Errorf("%+v", p)
			}
		}, false},
		{"token forces unlisted", "gated", `{"visibility":"public"}`, 200, func(t *testing.T, p *Paste) {
			if p.Visibility != VisUnlisted {
				t.Errorf("visibility = %q, want unlisted for a token-gated paste", p.Visibility)
			}
		}, false},
		{"unknown visibility", "old", `{"visibility":"everyone"}`, 200, func(t *testing.T, p *Paste) {
			if p.Visibility != VisUnlisted {
				t.Errorf("visibility = %q, want the create fallback, unlisted", p.Visibility)
			}
		}, false},
		{"private", "old", `{"visibility":"private"}`, 200, func(t *testing.T, p *Paste) {
			if p.Visibility != VisPrivate {
				t.Errorf("visibility = %q", p.Visibility)
			}
		}, false},
		{"expiry set", "old", `{"expires":"1d"}`, 200, func(t *testing.T, p *Paste) {
			if p.ExpiresAt == "" || p.Title != "old" {
				t.Errorf("%+v", p)
			}
		}, false},
		{"bad expiry", "old", `{"expires":"2y"}`, 400, nil, true},
		{"bad json", "old", `{`, 400, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ts := newTestServer(t)
			old, gated, _ := seedPastes(t, s)
			target := map[string]*Paste{"old": old, "gated": gated}[tc.target]
			resp, body := scrapAdmin(t, ts, "PUT", "/api/admin/pastes/"+target.ID, "k1", nil, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
			stored, _ := getPaste(s.db, target.ID)
			if tc.unchanged {
				if stored.Title != target.Title || stored.Visibility != target.Visibility || stored.ExpiresAt != target.ExpiresAt {
					t.Errorf("a refused update changed the paste: %+v", stored)
				}
				return
			}
			var got Paste
			_ = json.Unmarshal(body, &got)
			if got.Body != target.Body {
				t.Errorf("response body = %q, want the paste body", got.Body)
			}
			tc.check(t, stored)
			// The PUT's ETag is the one the admin GET now sends.
			getResp, _ := scrapAdmin(t, ts, "GET", "/api/admin/pastes/"+target.ID, "k1", nil, "")
			if getResp.Header.Get("ETag") != resp.Header.Get("ETag") {
				t.Errorf("PUT ETag %q != GET ETag %q", resp.Header.Get("ETag"), getResp.Header.Get("ETag"))
			}
		})
	}

	s, ts := newTestServer(t)
	seedPastes(t, s)
	if resp, _ := scrapAdmin(t, ts, "PUT", "/api/admin/pastes/abcdefghijklmnop", "k1", nil, `{"title":"x"}`); resp.StatusCode != 404 {
		t.Errorf("update missing = %d, want 404", resp.StatusCode)
	}
}
