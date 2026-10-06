package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

func qrDo(t *testing.T, ts *httptest.Server, method, path, key string, headers map[string]string, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-API-Key", key)
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

// seedHidden stores a code the public API never shows: private and disabled.
func seedHidden(t *testing.T, s *Server) *Code {
	t.Helper()
	c := &Code{Label: "hidden", Mode: ModeDirect, Target: "https://example.com/x",
		EC: "M", Public: false, Enabled: false, AdminNotes: "internal"}
	if err := insertCode(s.db, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAdminCodesGate(t *testing.T) {
	s := newTestServer(t)
	s.auth.ReadKey = "r1"
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	for _, tc := range []struct {
		name    string
		key     string
		headers map[string]string
		want    int
	}{
		{"tunnel", "k1", map[string]string{"Cf-Ray": "x"}, 404},
		{"tunnel client ip", "k1", map[string]string{"Cf-Connecting-Ip": "203.0.113.4"}, 404},
		{"wrong key", "nope", nil, 401},
		{"read key", "r1", nil, 401},
		{"write key", "k1", nil, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if resp, body := qrDo(t, ts, "GET", "/api/admin/codes", tc.key, tc.headers, ""); resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
		})
	}
}

func TestAdminCodes(t *testing.T) {
	s := newTestServer(t)
	c := seedHidden(t, s)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	resp, body := qrDo(t, ts, "GET", "/api/admin/codes", "k1", nil, "")
	var list struct{ Codes []Code }
	_ = json.Unmarshal(body, &list)
	if resp.StatusCode != 200 || len(list.Codes) != 1 || list.Codes[0].AdminNotes != "internal" {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}
	resp, body = qrDo(t, ts, "GET", "/api/admin/codes/"+c.ID, "k1", nil, "")
	if resp.StatusCode != 200 || resp.Header.Get("ETag") != `"`+c.CID+`"` || !strings.Contains(string(body), "internal") {
		t.Errorf("get = %d %s ETag %q", resp.StatusCode, body, resp.Header.Get("ETag"))
	}
	if resp, _ := qrDo(t, ts, "GET", "/api/codes/"+c.ID, "k1", nil, ""); resp.StatusCode != 404 {
		t.Errorf("public GET of a private code = %d — the public API changed", resp.StatusCode)
	}

	resp, body = qrDo(t, ts, "GET", "/api/admin/codes/"+c.ID+"/preview.svg", "k1", nil, "")
	if resp.StatusCode != 200 || !strings.HasPrefix(string(body), "<svg") ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "image/svg+xml") {
		t.Errorf("svg preview = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	for _, tc := range []struct {
		query    string
		min, max int
	}{
		{"", 400, 512},              // default 512
		{"?size=10", 25, 64},        // clamped up to 64; whole modules, so at or under it
		{"?size=99999", 1800, 2048}, // clamped down to 2048
		{"?size=300", 250, 300},
		{"?size=junk", 400, 512},
	} {
		resp, body := qrDo(t, ts, "GET", "/api/admin/codes/"+c.ID+"/preview.png"+tc.query, "k1", nil, "")
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("png%s = %d %q", tc.query, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		img, err := png.Decode(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("png%s: %v", tc.query, err)
		}
		if w := img.Bounds().Dx(); w < tc.min || w > tc.max {
			t.Errorf("png%s width = %d, want %d..%d", tc.query, w, tc.min, tc.max)
		}
	}
	if resp, _ := qrDo(t, ts, "GET", "/api/admin/codes/nope/preview.png", "k1", nil, ""); resp.StatusCode != 404 {
		t.Errorf("missing preview = %d, want 404", resp.StatusCode)
	}
}

func TestCodesIfMatch(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		ifMatch string // "" absent; "current" the code's tag
		want    int
	}{
		{"put absent", "PUT", "", 200},
		{"put current", "PUT", "current", 200},
		{"put stale", "PUT", `"bafystale"`, 412},
		{"delete current", "DELETE", "current", 200},
		{"delete stale", "DELETE", `W/"bafystale"`, 412},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			c := seedHidden(t, s)
			ts := httptest.NewServer(s.routes())
			defer ts.Close()
			etag := `"` + c.CID + `"`
			h := map[string]string{}
			switch tc.ifMatch {
			case "":
			case "current":
				h["If-Match"] = etag
			default:
				h["If-Match"] = tc.ifMatch
			}
			resp, body := qrDo(t, ts, tc.method, "/api/codes/"+c.ID, "k1", h, `{"label":"renamed"}`)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
			if tc.want == 412 {
				var pf struct{ Current Code }
				_ = json.Unmarshal(body, &pf)
				if pf.Current.Label != "hidden" || pf.Current.AdminNotes != "internal" || resp.Header.Get("ETag") != etag {
					t.Errorf("412 = %s ETag %q", body, resp.Header.Get("ETag"))
				}
				return
			}
			if tc.method == "PUT" {
				get, _ := qrDo(t, ts, "GET", "/api/admin/codes/"+c.ID, "k1", nil, "")
				if resp.Header.Get("ETag") == etag || resp.Header.Get("ETag") != get.Header.Get("ETag") {
					t.Errorf("PUT ETag %q, GET ETag %q, old %q", resp.Header.Get("ETag"), get.Header.Get("ETag"), etag)
				}
			}
		})
	}
}

func TestCodesIfMatchConcurrent(t *testing.T) {
	s := newTestServer(t)
	c := seedHidden(t, s)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	// Several rounds: one round can serialize by luck and pass without the
	// guard on the UPDATE; ten in a row do not.
	for round := 0; round < 10; round++ {
		cur, _ := getCode(s.db, c.ID)
		etag := `"` + cur.CID + `"`
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, _ := qrDo(t, ts, "PUT", "/api/codes/"+c.ID, "k1",
					map[string]string{"If-Match": etag},
					`{"label":"round `+string(rune('a'+round))+` writer `+string(rune('0'+i))+`"}`)
				codes[i] = resp.StatusCode
			}()
		}
		wg.Wait()
		slices.Sort(codes)
		if codes[0] != 200 || codes[1] != 412 {
			t.Fatalf("round %d: statuses = %v, want exactly one 200 and one 412", round, codes)
		}
	}
}
