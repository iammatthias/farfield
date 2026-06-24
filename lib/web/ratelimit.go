package web

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimiter is a per-key sliding-window request limiter, safe for concurrent
// use. Keys are typically client IPs. It keeps the timestamps of recent hits
// per key and refuses a key once it reaches max hits inside the window.
type RateLimiter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	hits   map[string][]time.Time
	now    func() time.Time
}

// NewRateLimiter returns a limiter allowing max requests per key per window.
func NewRateLimiter(max int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		window: window,
		max:    max,
		hits:   make(map[string][]time.Time),
		now:    time.Now,
	}
}

// prune drops timestamps older than the window. Callers hold mu.
func (l *RateLimiter) prune(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = kept
	return kept
}

// Allow records a hit for key and reports whether it stayed within budget. A
// rejected request is not recorded, so a blocked key recovers as soon as its
// oldest in-window hit ages out.
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Bound the map against a caller cycling keys; a reset forgives in-flight
	// counts, which is acceptable for a personal service.
	if len(l.hits) > 8192 {
		l.hits = make(map[string][]time.Time)
	}
	kept := l.prune(key)
	if len(kept) >= l.max {
		return false
	}
	l.hits[key] = append(kept, l.now())
	return true
}

// RateLimit wraps next with a per-client-IP request limiter. Requests for which
// exempt returns true skip the limiter entirely (e.g. those carrying a valid
// API key); a nil exempt limits everyone. Over-budget requests get a 429 with
// a Retry-After hint. A nil limiter disables limiting — handy in tests.
func RateLimit(l *RateLimiter, exempt func(*http.Request) bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if l != nil && (exempt == nil || !exempt(r)) {
			if !l.Allow(ClientIP(r)) {
				w.Header().Set("Retry-After", strconv.Itoa(int(l.window.Seconds())))
				WriteError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
		}
		next(w, r)
	}
}

// ClientIP resolves the client address for rate-limit keying: the Cloudflare
// header when present, else the first X-Forwarded-For hop, else the socket
// peer. farfield runs behind a Cloudflare tunnel, so CF-Connecting-IP carries
// the real remote address.
func ClientIP(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
