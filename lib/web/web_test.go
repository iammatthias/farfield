package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestLogRequestsCapturesStatus(t *testing.T) {
	// The recorder must forward the handler's status untouched.
	h := LogRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", w.Code)
	}
}

func TestCORSPreflight(t *testing.T) {
	h := CORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("preflight must not reach the handler")
	}), "GET", "POST")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("OPTIONS", "/api/things", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST" {
		t.Fatalf("Allow-Methods = %q", got)
	}
	if w.Header().Get("Access-Control-Max-Age") == "" {
		t.Fatal("missing Access-Control-Max-Age on preflight")
	}
}

func TestGzipCompressesJSON(t *testing.T) {
	body := strings.Repeat(`{"key":"value"}`, 100)
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	r := httptest.NewRequest("GET", "/api", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	out, _ := io.ReadAll(zr)
	if string(out) != body {
		t.Fatal("body does not round-trip")
	}
}

func TestGzipSkips(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *http.Request, w http.ResponseWriter)
	}{
		{"binary content type", func(r *http.Request, w http.ResponseWriter) {
			w.Header().Set("Content-Type", "image/jpeg")
		}},
		{"already encoded", func(r *http.Request, w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Encoding", "br")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tc.setup(r, w)
				io.WriteString(w, "payload")
			}))
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Accept-Encoding", "gzip")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Body.String() != "payload" && w.Header().Get("Content-Encoding") == "gzip" {
				t.Fatal("response was compressed but should not be")
			}
		})
	}

	t.Run("range request passes through", func(t *testing.T) {
		h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "payload")
		}))
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		r.Header.Set("Range", "bytes=0-3")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Header().Get("Content-Encoding") == "gzip" {
			t.Fatal("Range request was compressed")
		}
	})

	t.Run("304 stays empty", func(t *testing.T) {
		h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotModified)
		}))
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Body.Len() != 0 || w.Header().Get("Content-Encoding") == "gzip" {
			t.Fatal("304 must not carry a gzip body")
		}
	})
}

func TestETagMatch(t *testing.T) {
	etag := "bafyexample"
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{`"bafyexample"`, true},
		{`W/"bafyexample"`, true},
		{`"other", "bafyexample"`, true},
		{`"other", W/"bafyexample"`, true},
		{"*", true},
		{`"nope"`, false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if tc.header != "" {
			r.Header.Set("If-None-Match", tc.header)
		}
		if got := ETagMatch(r, etag); got != tc.want {
			t.Fatalf("ETagMatch(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestWriteRecord304(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("If-None-Match", `W/"abc"`)
	w := httptest.NewRecorder()
	WriteRecord(w, r, "abc", map[string]string{"x": "y"})
	if w.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Fatal("304 must not carry a body")
	}
}

func TestParseTemplatesAndRender(t *testing.T) {
	fsys := fstest.MapFS{
		"templates/base.html": {Data: []byte(
			`{{define "base"}}<html>v={{.AssetVer}} {{template "content" .}}</html>{{end}}`)},
		"templates/page.html": {Data: []byte(
			`{{define "content"}}hello {{.Name}}{{end}}`)},
	}
	tmpl, err := ParseTemplates(fsys, nil)
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	rd := &Renderer{Templates: tmpl, AssetVer: "v1"}
	w := httptest.NewRecorder()
	rd.Render(w, "page.html", map[string]any{"Name": "farfield"})
	got := w.Body.String()
	if !strings.Contains(got, "hello farfield") || !strings.Contains(got, "v=v1") {
		t.Fatalf("rendered %q", got)
	}
}

func TestAPIKeyFrom(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "k1")
	if APIKeyFrom(r) != "k1" {
		t.Fatal("X-API-Key not read")
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer k2")
	if APIKeyFrom(r) != "k2" {
		t.Fatal("Bearer token not read")
	}
}
