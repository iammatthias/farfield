package web

import (
	"net/http"
	"net/url"
	"sync"
	"time"
)

// FailLimiter rate-limits *failed* attempts per key — typically per client IP,
// or per (IP, record). Only failures count: a correct credential can be
// replayed freely (a magic link, a login), but once maxFails failures land
// inside the window, further attempts for that key are refused until the
// oldest failure ages out. Safe for concurrent use.
//
// It complements RateLimiter, which counts every request; use FailLimiter for
// guessable secrets (tokens, passwords) where legitimate traffic must never
// throttle but brute force must.
type FailLimiter struct {
	mu       sync.Mutex
	window   time.Duration
	maxFails int
	fails    map[string][]time.Time
	now      func() time.Time
}

// NewFailLimiter returns a limiter allowing maxFails failures per key per
// window.
func NewFailLimiter(maxFails int, window time.Duration) *FailLimiter {
	return &FailLimiter{
		window:   window,
		maxFails: maxFails,
		fails:    make(map[string][]time.Time),
		now:      time.Now,
	}
}

// prune drops entries older than the window. Callers hold mu.
func (l *FailLimiter) prune(key string) []time.Time {
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

// Blocked reports whether the key has exhausted its failure budget.
func (l *FailLimiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key)) >= l.maxFails
}

// Fail records one failed attempt.
func (l *FailLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Bound the map against an attacker cycling keys; resetting forgives
	// in-flight counts, which is acceptable for a personal service.
	if len(l.fails) > 4096 {
		l.fails = make(map[string][]time.Time)
	}
	l.fails[key] = append(l.prune(key), l.now())
}

// FailLimit wraps a login-style form handler: over-budget clients get a flat
// 429 before the handler runs, and posts that redirect back with an ?error=
// query — the farfield login-failure convention — count as failures. It
// exists so a session login can be brute-force-limited in one line.
func FailLimit(l *FailLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r)
		if l.Blocked(ip) {
			http.Error(w, "too many attempts — try again shortly", http.StatusTooManyRequests)
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		next(rec, r)
		// A failed login redirects back with ?error=...; success redirects
		// without one. Only the failure consumes budget.
		if rec.status == http.StatusSeeOther {
			if u, err := url.Parse(rec.Header().Get("Location")); err == nil &&
				u.Query().Has("error") {
				l.Fail(ip)
			}
		}
	}
}
