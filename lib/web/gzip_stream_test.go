package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A streaming handler must reach the client incrementally rather than sitting
// in the compressor's buffer until the handler returns. Before gzipWriter had
// a Flush method, the wrapper silently swallowed every flush.
func TestGzipWriterForwardsFlush(t *testing.T) {
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("wrapped writer does not implement http.Flusher")
			return
		}
		_, _ = io.WriteString(w, "first chunk")
		f.Flush()
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	// The flushed bytes must be decodable on their own — that is what a
	// streaming client sees before the handler finishes.
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != "first chunk" {
		t.Errorf("body = %q, want %q", got, "first chunk")
	}
}

// http.ResponseController reaches the real connection through wrappers only
// when each one exposes Unwrap.
func TestWrappersUnwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	gw := &gzipWriter{ResponseWriter: inner}
	if gw.Unwrap() != http.ResponseWriter(inner) {
		t.Error("gzipWriter.Unwrap did not return the wrapped writer")
	}
	sr := &statusRecorder{ResponseWriter: inner}
	if sr.Unwrap() != http.ResponseWriter(inner) {
		t.Error("statusRecorder.Unwrap did not return the wrapped writer")
	}
}

// GzipExcept lets an app compress its HTML and JSON while leaving raw object
// bytes — which must reach ServeContent untouched — alone.
func TestGzipExceptSkipsMatchingPaths(t *testing.T) {
	body := strings.Repeat("compress me ", 100)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, body)
	})
	h := GzipExcept(inner, PathPrefixSkipper("/raw/"))

	for _, tt := range []struct {
		path       string
		wantGzip   bool
		whatItMean string
	}{
		{"/admin", true, "an HTML route should compress"},
		{"/raw/abc123", false, "a raw-bytes route must pass through"},
	} {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		gotGzip := rec.Header().Get("Content-Encoding") == "gzip"
		if gotGzip != tt.wantGzip {
			t.Errorf("%s: %s: Content-Encoding = %q",
				tt.path, tt.whatItMean, rec.Header().Get("Content-Encoding"))
		}
		if !gotGzip && rec.Body.String() != body {
			t.Errorf("%s: uncompressed body was altered", tt.path)
		}
	}
}
