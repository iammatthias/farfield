package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProxyGetRevalidates pins that a proxied list keeps its validator: the
// upstream's ETag reaches the browser, and the browser's If-None-Match
// reaches the upstream, so an unchanged list is a 304 end to end.
func TestProxyGetRevalidates(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k" {
			t.Errorf("api key not forwarded")
		}
		WriteJSONValidated(w, r, map[string]any{"blobs": []string{"a", "b"}})
	}))
	defer upstream.Close()

	get := func(inm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/embed/blobs", nil)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		rec := httptest.NewRecorder()
		ProxyGet(rec, req, upstream.URL, "k")
		return rec
	}

	first := get("")
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || etag == "" {
		t.Fatalf("first: %d etag %q", first.Code, etag)
	}
	again := get(etag)
	if again.Code != http.StatusNotModified {
		t.Fatalf("revalidation: %d, want 304", again.Code)
	}
	if again.Body.Len() != 0 {
		t.Errorf("304 carried a body: %q", again.Body.String())
	}
}
