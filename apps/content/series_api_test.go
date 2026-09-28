package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// PUT /api/series/{slug} rewrites a fragment in place: same slug, new body,
// new CID; the write key is required and a missing fragment is a 404.
func TestAPIUpdateSeries(t *testing.T) {
	s, _ := readTestServer(t)
	orig := &Series{Slug: "gallery", Title: "Gallery", Body: "![](blob://old)"}
	if err := upsertSeries(s.db, orig); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	put := func(slug, key, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/series/"+slug, strings.NewReader(body))
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if r := put("gallery", "", `{"body":"x"}`); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no key = %d, want 401", r.StatusCode)
	}
	if r := put("gallery", "read-secret", `{"body":"x"}`); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("read key = %d, want 401", r.StatusCode)
	}
	if r := put("nope", "write-secret", `{"body":"x"}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("missing series = %d, want 404", r.StatusCode)
	}
	if r := put("gallery", "write-secret", `{"title":"Gallery"}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("no body = %d, want 400", r.StatusCode)
	}

	r := put("gallery", "write-secret", `{"body":"![](blob://new)"}`)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("update = %d", r.StatusCode)
	}
	var got Series
	_ = json.NewDecoder(r.Body).Decode(&got)
	if got.Slug != "gallery" || got.Body != "![](blob://new)" || got.Title != "Gallery" {
		t.Errorf("updated = %+v", got)
	}
	if got.CID == orig.CID {
		t.Error("CID did not change with the body")
	}
	stored, _ := getSeries(s.db, "gallery")
	if stored.Body != "![](blob://new)" {
		t.Errorf("stored body = %q", stored.Body)
	}
}
