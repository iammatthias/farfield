// Package web provides the HTTP plumbing every farfield app needs —
// middleware, JSON writers, auth gates, template rendering, and the server
// lifecycle — so each app's server.go holds only its own routes and handlers.
package web

import (
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// statusRecorder captures the response status for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	return sr.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so streaming responses keep working.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// LogRequests logs every request with its method, path, response status, and
// duration.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		slog.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", time.Since(start))
	})
}

// CORS adds permissive CORS headers so a browser on another origin (the
// public website) can use the API, and answers preflight requests. Methods
// defaults to read-only (GET, OPTIONS); apps with browser-facing write APIs
// pass their full method list.
func CORS(next http.Handler, methods ...string) http.Handler {
	allow := "GET, OPTIONS"
	if len(methods) > 0 {
		allow = strings.Join(methods, ", ")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", allow)
		h.Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// gzipTypes lists the content-type prefixes worth compressing. Already-
// compressed media (images, EPUBs, archives) is excluded by omission.
var gzipTypes = []string{"text/", "application/json", "application/atom+xml", "image/svg+xml"}

type gzipWriter struct {
	http.ResponseWriter
	r           *http.Request
	zw          *gzip.Writer
	decided     bool
	wroteHeader bool
}

func (gw *gzipWriter) WriteHeader(code int) {
	if gw.wroteHeader {
		return
	}
	gw.wroteHeader = true
	gw.decide(code)
	gw.ResponseWriter.WriteHeader(code)
}

// decide inspects the response once, at first write, and turns compression on
// only when it is safe and worthwhile.
func (gw *gzipWriter) decide(code int) {
	if gw.decided {
		return
	}
	gw.decided = true
	h := gw.Header()
	if code == http.StatusNoContent || code == http.StatusNotModified ||
		h.Get("Content-Encoding") != "" || h.Get("Content-Range") != "" {
		return
	}
	ct := h.Get("Content-Type")
	compressible := false
	for _, t := range gzipTypes {
		if strings.HasPrefix(ct, t) {
			compressible = true
			break
		}
	}
	if !compressible {
		return
	}
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length") // the compressed length differs
	gw.zw = gzip.NewWriter(gw.ResponseWriter)
}

func (gw *gzipWriter) Write(b []byte) (int, error) {
	if !gw.wroteHeader {
		gw.WriteHeader(http.StatusOK)
	}
	if gw.zw != nil {
		return gw.zw.Write(b)
	}
	return gw.ResponseWriter.Write(b)
}

func (gw *gzipWriter) Close() error {
	if gw.zw != nil {
		return gw.zw.Close()
	}
	return nil
}

// Gzip compresses text, JSON, Atom, and SVG responses when the client accepts
// it. Range requests and already-encoded responses pass through untouched —
// do not wrap routes that serve raw blob/file bytes via http.ServeContent.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w, r: r}
		defer gw.Close()
		next.ServeHTTP(gw, r)
	})
}

var _ io.Closer = (*gzipWriter)(nil)
