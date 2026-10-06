package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// Under gzipMinSize compression only adds bytes (a 24-byte list went out as
// 49), so a small body must reach the client plain and unaltered — including
// a status the handler set explicitly, which the wrapper holds back while it
// decides.
func TestGzipLeavesSmallBodiesPlain(t *testing.T) {
	body := `{"pastes":[],"total":0}` + "\n"
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, body)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want none for a %d-byte body", ce, len(body))
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if rec.Body.String() != body {
		t.Errorf("body = %q, want %q", rec.Body.String(), body)
	}
}

// A body that crosses the threshold across several small writes is
// compressed whole — the bytes held back before the decision must not be lost
// or sent twice.
func TestGzipCompressesBodyCrossingThreshold(t *testing.T) {
	chunk := strings.Repeat("x", 100)
	var want strings.Builder
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for range 30 {
			_, _ = io.WriteString(w, chunk)
		}
	}))
	for range 30 {
		want.WriteString(chunk)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if ce := rec.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	if v := rec.Header().Get("Vary"); v != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", v)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != want.String() {
		t.Errorf("decoded %d bytes, want %d", len(got), want.Len())
	}
}

// ReadFrom with an unknown size goes through the same threshold: small plain,
// large compressed.
func TestGzipReadFromThreshold(t *testing.T) {
	for _, tt := range []struct {
		size     int
		wantGzip bool
	}{{10, false}, {5000, true}} {
		body := strings.Repeat("r", tt.size)
		h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.(io.ReaderFrom).ReadFrom(strings.NewReader(body))
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		gotGzip := rec.Header().Get("Content-Encoding") == "gzip"
		if gotGzip != tt.wantGzip {
			t.Fatalf("size %d: gzip = %v, want %v", tt.size, gotGzip, tt.wantGzip)
		}
		got := rec.Body.String()
		if gotGzip {
			zr, err := gzip.NewReader(rec.Body)
			if err != nil {
				t.Fatalf("gzip.NewReader: %v", err)
			}
			b, _ := io.ReadAll(zr)
			got = string(b)
		}
		if got != body {
			t.Errorf("size %d: body altered (%d bytes)", tt.size, len(got))
		}
	}
}

// A flush before the threshold commits to compression (a flushing handler is
// streaming, its size unknown), and every chunk must reach the client at its
// flush — later writes continue the same gzip stream.
func TestGzipFlushBeforeThresholdStreams(t *testing.T) {
	var atFlush int
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rec := w.(*gzipWriter).ResponseWriter.(*httptest.ResponseRecorder)
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		atFlush = rec.Body.Len()
		_, _ = io.WriteString(w, "data: two\n\n")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if atFlush == 0 {
		t.Fatal("nothing reached the client at Flush; the bytes sat in the threshold buffer")
	}
	if !rec.Flushed {
		t.Error("Flush was not forwarded to the connection")
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != "data: one\n\ndata: two\n\n" {
		t.Errorf("body = %q", got)
	}
}

// A handler that serves its own precompressed bytes (the editor's assets)
// must pass through byte for byte: one Content-Encoding, no second gzip
// layer, its own Content-Length kept.
func TestGzipPassesPrecompressedThrough(t *testing.T) {
	var pre bytes.Buffer
	zw := gzip.NewWriter(&pre)
	_, _ = io.WriteString(zw, strings.Repeat("precompressed ", 500))
	_ = zw.Close()
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(pre.Len()))
		_, _ = w.Write(pre.Bytes())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if ce := rec.Header().Values("Content-Encoding"); len(ce) != 1 || ce[0] != "gzip" {
		t.Errorf("Content-Encoding = %q, want exactly [gzip]", ce)
	}
	if rec.Header().Get("Content-Length") != strconv.Itoa(pre.Len()) {
		t.Errorf("Content-Length = %q, want %d", rec.Header().Get("Content-Length"), pre.Len())
	}
	if !bytes.Equal(rec.Body.Bytes(), pre.Bytes()) {
		t.Error("precompressed body was altered")
	}
}
