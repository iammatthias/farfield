package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iammatthias/farfield/lib/cid"
)

func TestCheckIfMatch(t *testing.T) {
	const etag = "bafycurrent"
	cases := []struct {
		name   string
		header string
		want   Precondition
	}{
		{"absent", "", Unconditional},
		{"star", "*", Unconditional},
		{"quoted", `"bafycurrent"`, Matched},
		{"unquoted", `bafycurrent`, Matched},
		{"weak", `W/"bafycurrent"`, Matched},
		{"in a list", `"bafyold", W/"bafycurrent"`, Matched},
		{"stale", `"bafyold"`, Failed},
		{"stale list", `"bafyold", "bafyolder"`, Failed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("PUT", "/api/x/1", nil)
			if tc.header != "" {
				r.Header.Set("If-Match", tc.header)
			}
			w := httptest.NewRecorder()
			got := CheckIfMatch(w, r, etag, map[string]string{"cid": etag})
			if got != tc.want {
				t.Fatalf("CheckIfMatch = %v, want %v", got, tc.want)
			}
			if got != Failed {
				if w.Body.Len() != 0 || w.Code != http.StatusOK {
					t.Fatalf("a passing check wrote a response: %d %s", w.Code, w.Body)
				}
				return
			}
			if w.Code != http.StatusPreconditionFailed {
				t.Fatalf("status = %d, want 412", w.Code)
			}
			if w.Header().Get("ETag") != `"`+etag+`"` {
				t.Errorf("ETag = %q, want the current tag", w.Header().Get("ETag"))
			}
			var body struct {
				Error   string            `json:"error"`
				Current map[string]string `json:"current"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error != "precondition failed" || body.Current["cid"] != etag {
				t.Errorf("body = %s", w.Body)
			}
		})
	}
}

// If-None-Match keeps its old semantics after the shared matcher was split out.
func TestETagMatchUnchanged(t *testing.T) {
	for header, want := range map[string]bool{
		"": false, "*": true, `"a"`: true, `W/"a"`: true, `"b", "a"`: true, `"b"`: false,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("If-None-Match", header)
		}
		if got := ETagMatch(r, "a"); got != want {
			t.Errorf("ETagMatch(%q) = %v, want %v", header, got, want)
		}
	}
}

// A list read's ETag is the CID of the exact bytes sent, so it changes when
// and only when the body does, and a client holding it gets a bodiless 304.
// The body must stay byte-identical to WriteJSON's.
func TestWriteJSONValidated(t *testing.T) {
	v := map[string]any{"items": []string{"a", "b"}}
	rec := httptest.NewRecorder()
	WriteJSONValidated(rec, httptest.NewRequest(http.MethodGet, "/", nil), v)
	plain := httptest.NewRecorder()
	WriteJSON(plain, http.StatusOK, v)

	if rec.Code != http.StatusOK || rec.Body.String() != plain.Body.String() {
		t.Fatalf("got %d %q, want 200 %q", rec.Code, rec.Body.String(), plain.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("Cache-Control = %q; the route owns it, the helper must not set one", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag != `"`+cid.Of(rec.Body.Bytes())+`"` {
		t.Fatalf("ETag = %s, want the CID of the body", etag)
	}

	for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("If-None-Match", inm)
		rec := httptest.NewRecorder()
		WriteJSONValidated(rec, req, v)
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Errorf("If-None-Match %s: got %d with %d bytes, want bodiless 304", inm, rec.Code, rec.Body.Len())
		}
		if rec.Header().Get("ETag") != etag {
			t.Errorf("If-None-Match %s: 304 lost its ETag", inm)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	WriteJSONValidated(rec, req, map[string]any{"items": []string{"a", "b", "c"}})
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == etag {
		t.Errorf("changed body: got %d, ETag %s; want 200 with a new tag", rec.Code, rec.Header().Get("ETag"))
	}
}
