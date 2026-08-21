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

// Unwrap exposes the wrapped writer to http.ResponseController, so hijacking
// and deadline control reach the real connection through this wrapper.
func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

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

// Flush pushes the compressor's buffered bytes out and then flushes the
// connection, so a streaming handler (SSE, a progress log) reaches the client
// incrementally instead of stalling inside the gzip buffer.
func (gw *gzipWriter) Flush() {
	if !gw.wroteHeader {
		gw.WriteHeader(http.StatusOK)
	}
	if gw.zw != nil {
		_ = gw.zw.Flush()
	}
	if f, ok := gw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom keeps the kernel's sendfile path available for uncompressed
// responses — http.ServeContent copies through it, and without this method the
// wrapper would force every byte through userspace.
func (gw *gzipWriter) ReadFrom(src io.Reader) (int64, error) {
	if !gw.wroteHeader {
		gw.WriteHeader(http.StatusOK)
	}
	if gw.zw != nil {
		return io.Copy(gw.zw, src)
	}
	if rf, ok := gw.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	// Copy through Write, not through this type, or ReadFrom recurses.
	return io.Copy(struct{ io.Writer }{gw.ResponseWriter}, src)
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (gw *gzipWriter) Unwrap() http.ResponseWriter { return gw.ResponseWriter }

func (gw *gzipWriter) Close() error {
	if gw.zw != nil {
		return gw.zw.Close()
	}
	return nil
}

// GzipExcept is Gzip with an escape hatch: requests for which skip returns
// true bypass the compressor entirely. It exists for the apps that serve raw
// object bytes alongside an admin UI — blobs and library — which otherwise
// have to choose between compressing their HTML and JSON or leaving their
// byte routes alone. A nil skip is plain Gzip.
func GzipExcept(next http.Handler, skip func(*http.Request) bool) http.Handler {
	gz := Gzip(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip != nil && skip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gz.ServeHTTP(w, r)
	})
}

// PathPrefixSkipper returns a skip function matching any of the given path
// prefixes — the usual argument to GzipExcept.
func PathPrefixSkipper(prefixes ...string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				return true
			}
		}
		return false
	}
}

// Gzip compresses text, JSON, Atom, and SVG responses when the client accepts
// it. Range requests and already-encoded responses pass through untouched.
// Content-Type decides: raw blob bytes served via http.ServeContent are
// classified as images/video/octet-stream and skipped already, but prefer
// GzipExcept for routes that stream large objects, so they never enter the
// wrapper at all.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w, r: r}
		// A failed Close means the compressed tail never reached the client,
		// so the body is truncated under an already-sent 200. Nothing can
		// repair the response at this point — log it so it is visible.
		defer func() {
			if err := gw.Close(); err != nil {
				slog.Error("gzip close", "path", r.URL.Path, "err", err)
			}
		}()
		next.ServeHTTP(gw, r)
	})
}

var (
	_ io.Closer     = (*gzipWriter)(nil)
	_ io.ReaderFrom = (*gzipWriter)(nil)
	_ http.Flusher  = (*gzipWriter)(nil)
)

// SecureHeaders sets the response headers every app in the fleet should send
// and, before this, none of them did.
//
// The audit found no security headers on any response fleet-wide, which is a
// gap rather than an oversight in any one app: these services are reachable
// from the public internet through the Cloudflare tunnel, and several of them
// serve author-supplied content (markdown bodies, uploaded blobs, paste
// bodies) on origins that share a cookie domain with every admin UI.
//
// The set is deliberately conservative, because a wrong header here breaks
// fifteen apps at once:
//
//   - nosniff, so a mislabelled response is never sniffed into active HTML.
//   - SAMEORIGIN framing, so an admin console cannot be put in someone
//     else's iframe and clickjacked. Not DENY: the fleet frames its own
//     pages (the blobs viewer, sideload's install flow).
//   - A referrer policy that keeps paths off other origins. Capability URLs
//     live in paths here (sideload's /i/{token}), so a full referrer is a
//     credential leak.
//   - An opt-out of the legacy interest-cohort API.
//
// No Content-Security-Policy: the apps use inline styles and inline scripts
// widely enough that a policy strict enough to be worth having would have to
// be written per app, and a permissive one would only look like protection.
// That belongs in a separate pass with per-app nonces.
func SecureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "interest-cohort=()")
		next.ServeHTTP(w, r)
	})
}

// MaxBody caps how much of a request body a handler will read.
//
// Write endpoints across the fleet decoded bodies straight from r.Body, so
// any authenticated — and on a few routes, unauthenticated — caller could
// make a service allocate without limit. http.MaxBytesReader answers with a
// 413 and stops reading, so the cost is bounded before the handler sees it.
//
// Applied to unsafe methods only: a GET has no body worth capping, and
// wrapping one would add work to the read path that carries the traffic.
func MaxBody(next http.Handler, limit int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// DefaultMaxBody is the fleet's standard request-body ceiling. Generous for
// a form or a JSON document, far below what an uncapped decoder would accept.
// Apps that genuinely stream large uploads (blobs, library, sideload) set
// their own limits on those routes and must not be wrapped at this size.
const DefaultMaxBody = 2 << 20 // 2 MiB
