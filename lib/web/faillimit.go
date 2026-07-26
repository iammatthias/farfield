package web

import (
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

// There is deliberately no FailLimit middleware for logins: Auth.HandleLogin
// throttles itself (see lib/web/auth.go), so brute-force protection cannot be
// lost by forgetting to wrap a route. Use FailLimiter directly for the other
// guessable secrets — paste tokens, magic links — where the key is something
// richer than the client IP alone.
