package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
