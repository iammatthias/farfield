package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPIEntriesSlim: ?bodies=0 strips bodies, keeps everything else, and
// never shares an ETag with the full representation — a slim copy must not
// satisfy a full request's revalidation.
func TestAPIEntriesSlim(t *testing.T) {
	s, _ := readTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	fetch := func(path string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer read-secret")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, body
	}

	fullResp, fullBody := fetch("/api/entries")
	slimResp, slimBody := fetch("/api/entries?bodies=0")
	if fullResp.StatusCode != http.StatusOK || slimResp.StatusCode != http.StatusOK {
		t.Fatalf("status full=%d slim=%d", fullResp.StatusCode, slimResp.StatusCode)
	}
	if fullResp.Header.Get("ETag") == slimResp.Header.Get("ETag") {
		t.Fatal("slim and full lists share an ETag")
	}

	var out struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(slimBody, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Entries) == 0 {
		t.Fatal("slim list empty")
	}
	for _, e := range out.Entries {
		if e.Body != "" {
			t.Fatalf("slim entry %q kept its body", e.Slug)
		}
		if e.Slug == "" || e.Title == "" || e.CID == "" {
			t.Fatalf("slim entry dropped more than the body: %+v", e)
		}
	}
	if !strings.Contains(string(fullBody), `"body":"live"`) {
		t.Fatal("full variant lost its body")
	}
}
