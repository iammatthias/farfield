package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/iammatthias/farfield/lib/web"
)

func conditionalFeed(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "feed.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{db: db, auth: &web.Auth{DB: db, APIKey: "write"}}
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return srv
}

func feedDo(t *testing.T, srv *httptest.Server, method, path, ifMatch, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-API-Key", "write")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// The ETag a write returns must be the one a GET of the same post sends, or a
// client chaining conditional writes off the response would always 412.
func TestFeedWriteETagsMatchGET(t *testing.T) {
	srv := conditionalFeed(t)
	resp, body := feedDo(t, srv, "POST", "/api/posts", "", `{"body":"first","tags":["a"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var p Post
	_ = json.Unmarshal(body, &p)
	get, _ := feedDo(t, srv, "GET", "/api/posts/"+p.Slug, "", "")
	if resp.Header.Get("ETag") == "" || resp.Header.Get("ETag") != get.Header.Get("ETag") {
		t.Errorf("create ETag %q != GET ETag %q", resp.Header.Get("ETag"), get.Header.Get("ETag"))
	}

	// No If-Match: the old unconditional write, now with real timestamps.
	put, body := feedDo(t, srv, "PUT", "/api/posts/"+p.Slug, "", `{"body":"second"}`)
	if put.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %s", put.StatusCode, body)
	}
	var got Post
	_ = json.Unmarshal(body, &got)
	if got.CreatedAt != p.CreatedAt || got.UpdatedAt == "" || got.Slug != p.Slug {
		t.Errorf("PUT response = %+v — createdAt/updatedAt/slug must be the stored values (created %q)", got, p.CreatedAt)
	}
	get, _ = feedDo(t, srv, "GET", "/api/posts/"+p.Slug, "", "")
	if put.Header.Get("ETag") != get.Header.Get("ETag") {
		t.Errorf("PUT ETag %q != GET ETag %q", put.Header.Get("ETag"), get.Header.Get("ETag"))
	}
}

func TestFeedIfMatch(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		ifMatch  func(etag string) string
		slug     string
		want     int
		mutated  bool
		wantCurr bool
	}{
		{"put current", "PUT", func(e string) string { return e }, "", 200, true, false},
		{"put weak current", "PUT", func(e string) string { return "W/" + e }, "", 200, true, false},
		{"put stale", "PUT", func(string) string { return `"bafystale"` }, "", 412, false, true},
		{"put star", "PUT", func(string) string { return "*" }, "", 200, true, false},
		{"put missing", "PUT", func(e string) string { return e }, "nope", 404, false, false},
		{"delete current", "DELETE", func(e string) string { return e }, "", 200, true, false},
		{"delete stale", "DELETE", func(string) string { return `"bafystale"` }, "", 412, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := conditionalFeed(t)
			created, body := feedDo(t, srv, "POST", "/api/posts", "", `{"body":"original"}`)
			var p Post
			_ = json.Unmarshal(body, &p)
			slug := p.Slug
			if tc.slug != "" {
				slug = tc.slug
			}
			etag := created.Header.Get("ETag")

			resp, body := feedDo(t, srv, tc.method, "/api/posts/"+slug, tc.ifMatch(etag), `{"body":"changed"}`)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
			if tc.wantCurr {
				var pf struct {
					Error   string `json:"error"`
					Current Post   `json:"current"`
				}
				_ = json.Unmarshal(body, &pf)
				if pf.Error != "precondition failed" || pf.Current.Body != "original" || resp.Header.Get("ETag") != etag {
					t.Errorf("412 = %s, ETag %q — want the current post and its tag", body, resp.Header.Get("ETag"))
				}
			}
			if tc.method == "PUT" && tc.want == 200 && resp.Header.Get("ETag") == etag {
				t.Error("a successful PUT kept the old ETag")
			}
			get, gbody := feedDo(t, srv, "GET", "/api/posts/"+p.Slug, "", "")
			switch {
			case !tc.mutated && (get.StatusCode != 200 || !strings.Contains(string(gbody), `"original"`)):
				t.Errorf("a refused write changed the post: %d %s", get.StatusCode, gbody)
			case tc.mutated && tc.method == "DELETE" && get.StatusCode != 404:
				t.Errorf("delete did not delete: %d", get.StatusCode)
			}
		})
	}
}

// Two writers holding the same version: exactly one wins, the other gets 412.
// The check alone cannot promise this — both pass it — so it pins the guard
// on the UPDATE itself.
func TestFeedIfMatchConcurrent(t *testing.T) {
	srv := conditionalFeed(t)
	for round := 0; round < 10; round++ {
		created, body := feedDo(t, srv, "POST", "/api/posts", "", `{"body":"race `+string(rune('a'+round))+`"}`)
		var p Post
		_ = json.Unmarshal(body, &p)
		etag := created.Header.Get("ETag")

		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, _ := feedDo(t, srv, "PUT", "/api/posts/"+p.Slug, etag, `{"body":"writer `+string(rune('0'+i))+`"}`)
				codes[i] = resp.StatusCode
			}()
		}
		wg.Wait()
		slices.Sort(codes)
		if codes[0] != http.StatusOK || codes[1] != http.StatusPreconditionFailed {
			t.Fatalf("round %d: statuses = %v, want exactly one 200 and one 412", round, codes)
		}
	}
}
