package main

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// ttlStore holds short-lived, single-use values in memory under a random id:
// passkey ceremony state (keyed by a cookie) and device-flow grants (keyed by
// the authorization code itself). Memory, not the database, on purpose —
// both live five minutes, and a restart dropping them only means "try again",
// which beats leaving one-time secrets on disk.
type ttlStore[T any] struct {
	mu  sync.Mutex
	ttl time.Duration
	max int
	m   map[string]ttlEntry[T]
	now func() time.Time
}

type ttlEntry[T any] struct {
	v       T
	expires time.Time
}

func newTTLStore[T any](ttl time.Duration, max int) *ttlStore[T] {
	return &ttlStore[T]{ttl: ttl, max: max, m: make(map[string]ttlEntry[T]), now: time.Now}
}

// put stores v under a fresh 32-byte random id and returns the id.
//
// Some puts are anonymous (anyone can begin a passkey login), so the store
// is bounded: expired entries go first, and if it is still full the entry
// closest to expiry is evicted. Evicting rather than refusing means a flood
// can at worst make the owner tap their passkey twice, never lock them out.
func (s *ttlStore[T]) put(v T) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	id := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.m) >= s.max {
		var oldest string
		var oldestAt time.Time
		for k, e := range s.m {
			if !now.Before(e.expires) {
				delete(s.m, k)
				continue
			}
			if oldest == "" || e.expires.Before(oldestAt) {
				oldest, oldestAt = k, e.expires
			}
		}
		if len(s.m) >= s.max && oldest != "" {
			delete(s.m, oldest)
		}
	}
	s.m[id] = ttlEntry[T]{v: v, expires: now.Add(s.ttl)}
	return id
}

// take removes the entry for id and returns it if it had not yet expired.
// Removal comes first and happens whatever the outcome: a value is good for
// exactly one attempt, so a wrong guess at its partner (a PKCE verifier, a
// signature) also burns it.
func (s *ttlStore[T]) take(id string) (T, bool) {
	var zero T
	if id == "" {
		return zero, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return zero, false
	}
	delete(s.m, id)
	if !s.now().Before(e.expires) {
		return zero, false
	}
	return e.v, true
}

// len is how many entries are held, expired or not — for tests.
func (s *ttlStore[T]) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
