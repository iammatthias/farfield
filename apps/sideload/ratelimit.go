package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenLimiter rate-limits failed token attempts per (client IP, paste id).
// Only failures count — a correct token in a magic link can be replayed
// freely — and once maxFails failures land inside the window, further
// attempts for that key are refused (429) until the oldest failure ages out.
type tokenLimiter struct {
	mu       sync.Mutex
	window   time.Duration
	maxFails int
	fails    map[string][]time.Time
	now      func() time.Time
}

func newTokenLimiter(maxFails int, window time.Duration) *tokenLimiter {
	return &tokenLimiter{
		window:   window,
		maxFails: maxFails,
		fails:    make(map[string][]time.Time),
		now:      time.Now,
	}
}

// prune drops entries older than the window. Callers hold mu.
func (l *tokenLimiter) prune(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	kept := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = kept
	return kept
}

// blocked reports whether the key has exhausted its failure budget.
func (l *tokenLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key)) >= l.maxFails
}

// fail records one failed attempt.
func (l *tokenLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Bound the map against an attacker cycling keys; resetting forgives
	// in-flight counts, which is acceptable for a personal service.
	if len(l.fails) > 4096 {
		l.fails = make(map[string][]time.Time)
	}
	l.fails[key] = append(l.prune(key), l.now())
}

// clientIP resolves the client address for rate-limit keying: the Cloudflare
// header when present, else the first X-Forwarded-For hop, else the socket
// peer.
func clientIP(r *http.Request) string {
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
