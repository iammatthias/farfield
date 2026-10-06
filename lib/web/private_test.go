package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPrivateAPIGate pins every outcome of the admin gate, including the
// ordering: a Cloudflare request is a 404 even when the app has no key at all,
// so not even the 503 tells the internet the route exists.
func TestPrivateAPIGate(t *testing.T) {
	keyed := &Auth{
		APIKey:  "env-write",
		ReadKey: "env-read",
		App:     "feed",
		Keys: fakeKeys{
			"ffk_w": {"feed", "write"},
			"ffk_r": {"feed", "read"},
			"ffk_u": {"feed", "upload"},
		},
	}
	unkeyed := &Auth{ReadKey: "env-read"}

	cases := []struct {
		name    string
		auth    *Auth
		key     string
		headers map[string]string
		want    int
		wantErr string
	}{
		{"cf-ray is not private ingress", keyed, "env-write", map[string]string{"Cf-Ray": "8a1b2c3d4e5f-SJC"}, 404, "not found"},
		{"cf-connecting-ip is not private ingress", keyed, "env-write", map[string]string{"CF-Connecting-IP": "203.0.113.9"}, 404, "not found"},
		{"cloudflare outranks no key configured", unkeyed, "", map[string]string{"Cf-Ray": "x"}, 404, "not found"},
		{"no key configured", unkeyed, "env-read", nil, 503, "admin API disabled: no key configured"},
		{"no key presented", keyed, "", nil, 401, "missing or invalid API key"},
		{"wrong key", keyed, "nope", nil, 401, "missing or invalid API key"},
		{"read key", keyed, "env-read", nil, 401, "missing or invalid API key"},
		{"minted read key", keyed, "ffk_r", nil, 401, "missing or invalid API key"},
		{"minted upload key", keyed, "ffk_u", nil, 401, "missing or invalid API key"},
		{"env write key", keyed, "env-write", nil, 200, ""},
		{"minted write key", keyed, "ffk_w", nil, 200, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.auth.PrivateAPI(func(w http.ResponseWriter, r *http.Request) {
				WriteRecord(w, r, "bafyetag", map[string]string{"ok": "yes"})
			})
			r := httptest.NewRequest("GET", "/api/admin/things", nil)
			if tc.key != "" {
				r.Header.Set("Authorization", "Bearer "+tc.key)
			}
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			if tc.wantErr == "" {
				if w.Header().Get("ETag") != `"bafyetag"` {
					t.Errorf("ETag = %q — the gate must not strip the record's validator", w.Header().Get("ETag"))
				}
				return
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != tc.wantErr {
				t.Errorf("body = %s, want error %q", w.Body, tc.wantErr)
			}
		})
	}
}

// An unknown admin path and a known one with the wrong method answer the same
// JSON 404 as a real route seen from the internet, rather than the mux's own
// plain-text 404 or 405.
func TestAdminNotFoundCatchAll(t *testing.T) {
	a := &Auth{APIKey: "k"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/things", a.PrivateAPI(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]any{})
	}))
	mux.HandleFunc(AdminPrefix, a.PrivateAPI(AdminNotFound))

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/admin/nothing"},
		{"POST", "/api/admin/things"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("X-API-Key", "k")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s %s = %d %q, want a JSON 404", tc.method, tc.path, w.Code, w.Header().Get("Content-Type"))
		}
	}
}
