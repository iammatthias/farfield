package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// sourceNASA tags every cached photo. The calendar has a single source — NASA's
// Astronomy Picture of the Day — but the cache still keys on (source, date).
const sourceNASA = "nasa"

// userAgent identifies the calendar service to the APOD upstream.
const userAgent = "farfield-calendar/1.0 (+https://farfield.systems)"

// Resilience tuning — these keep the service from hammering a rate-limited
// upstream. Cache-first reads mean APOD is touched only on a miss; on top of
// that a failure trips a cooldown and a negative cache.
const (
	nasaCooldown = 10 * time.Minute // pause upstream calls after a failure
	negativeTTL  = 2 * time.Hour    // remember a failed date for this long
)

// fetcher performs upstream HTTP calls and tracks the state that keeps the
// service resilient: a cooldown after failures and a per-date negative cache.
type fetcher struct {
	client  *http.Client
	nasaKey string
	flights flightGroup // dedups concurrent upstream calls for the same date/range

	mu             sync.Mutex
	nasaCooldownAt time.Time            // upstream NASA calls paused until here
	negative       map[string]time.Time // date -> when its fetch last failed
}

// newFetcher builds a fetcher with a bounded HTTP timeout.
func newFetcher(nasaKey string) *fetcher {
	return &fetcher{
		client:   &http.Client{Timeout: 20 * time.Second},
		nasaKey:  nasaKey,
		negative: map[string]time.Time{},
	}
}

// flightGroup deduplicates concurrent in-flight calls by key: the first caller
// performs the work, everyone else arriving before it finishes waits for and
// shares the same result. A hand-rolled singleflight — stdlib only.
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	done    chan struct{}
	records []apodResponse
	err     error
}

// do runs fn once per key at a time. Concurrent callers with the same key
// block until the in-flight call finishes and receive its result.
func (g *flightGroup) do(key string, fn func() ([]apodResponse, error)) ([]apodResponse, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*flight{}
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.records, c.err
	}
	c := &flight{done: make(chan struct{})}
	g.m[key] = c
	g.mu.Unlock()

	c.records, c.err = fn()
	close(c.done)

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	return c.records, c.err
}

// nasaAllowed reports whether NASA upstream calls are permitted right now —
// false while a post-failure cooldown is in effect.
func (f *fetcher) nasaAllowed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Now().After(f.nasaCooldownAt)
}

// noteNASAError starts a cooldown so a rate-limited or down upstream is not
// hammered by every subsequent request.
func (f *fetcher) noteNASAError() {
	f.mu.Lock()
	f.nasaCooldownAt = time.Now().Add(nasaCooldown)
	f.mu.Unlock()
}

// markNegative records that a date failed, so it is not retried on every
// request for the next negativeTTL.
func (f *fetcher) markNegative(date string) {
	f.mu.Lock()
	f.negative[date] = time.Now()
	f.mu.Unlock()
}

// negativeHit reports whether a date failed recently enough to skip retrying.
func (f *fetcher) negativeHit(date string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.negative[date]
	return ok && time.Since(at) < negativeTTL
}

// ── orchestration: cache-first reads with resilient upstream fallback ───────

// photoForDate returns the photo for a date, fetching it from APOD only on a
// cache miss. A failed fetch returns (nil, nil) — the caller renders a
// not-found rather than an error.
func (s *Server) photoForDate(ctx context.Context, date string) (*Photo, error) {
	p, err := getPhoto(s.db, sourceNASA, date)
	if err != nil || p != nil {
		return p, err
	}
	return s.nasaEnsureDay(ctx, date)
}

// todayPhoto returns the most recent photo. The cache answers first; APOD is
// asked for the current day only when the newest cached day is older — the
// current day's photo is sometimes posted late, in which case the newest
// cached day stands in.
func (s *Server) todayPhoto(ctx context.Context) (*Photo, string, error) {
	today := todayUTC()
	latest, err := latestPhoto(s.db, sourceNASA)
	if err != nil {
		return nil, today, err
	}
	if latest == nil || latest.Date < today {
		p, err := s.nasaEnsureDay(ctx, today)
		if err != nil {
			return nil, today, err
		}
		if p != nil {
			return p, today, nil
		}
	}
	if latest == nil {
		return nil, today, nil
	}
	return latest, latest.Date, nil
}

// nasaEnsureDay fetches and caches one APOD day, honouring the cooldown and
// negative cache. A miss or a failure yields (nil, nil).
func (s *Server) nasaEnsureDay(ctx context.Context, date string) (*Photo, error) {
	if !nasaDateInRange(date) || s.fetcher.negativeHit(date) {
		return nil, nil
	}
	if !s.fetcher.nasaAllowed() {
		return nil, nil // cooling down — serve cache only
	}
	p, err := s.fetcher.nasaDay(ctx, date)
	if err != nil {
		slog.Warn("nasa day fetch failed", "date", date, "err", err)
		s.fetcher.markNegative(date)
		s.fetcher.noteNASAError()
		return nil, nil
	}
	// upsertPhoto stamps the CID and fetch time on p, so p is already the
	// stored row — no re-read needed.
	if err := upsertPhoto(s.db, p); err != nil {
		return nil, err
	}
	return p, nil
}

// nasaEnsureRange warms the cache for a date range in a single upstream call,
// but only when the range is not already fully cached.
func (s *Server) nasaEnsureRange(ctx context.Context, start, end string) error {
	want := daysBetween(start, end)
	have, err := countPhotosBetween(s.db, sourceNASA, start, end)
	if err != nil {
		return err
	}
	if have >= want || want <= 0 {
		return nil // already warm
	}
	if !s.fetcher.nasaAllowed() {
		return nil // cooling down — serve whatever is cached
	}
	photos, err := s.fetcher.nasaRange(ctx, start, end)
	if err != nil {
		slog.Warn("nasa range fetch failed", "start", start, "end", end, "err", err)
		s.fetcher.noteNASAError()
		return nil
	}
	// One transaction for the whole page of days — a single fsync instead of
	// one autocommit per row.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for i := range photos {
		if err := upsertPhoto(tx, &photos[i]); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ── date helpers ────────────────────────────────────────────────────────────

// todayUTC returns the current UTC calendar date.
func todayUTC() string { return time.Now().UTC().Format(dateLayout) }

// validDate reports whether s is a well-formed YYYY-MM-DD date.
func validDate(s string) bool {
	_, err := time.Parse(dateLayout, s)
	return err == nil
}

// nasaDateInRange reports whether a date belongs to Farfield's public calendar:
// Jan 1 2026 through today, inclusive. Earlier APOD records exist, but are
// intentionally outside this app's archive.
func nasaDateInRange(date string) bool {
	d, err := time.Parse(dateLayout, date)
	if err != nil {
		return false
	}
	epoch, _ := time.Parse(dateLayout, calendarStart)
	today, _ := time.Parse(dateLayout, todayUTC())
	return !d.Before(epoch) && !d.After(today)
}

// daysBetween returns the inclusive day count of a [start, end] range.
func daysBetween(start, end string) int {
	s, err1 := time.Parse(dateLayout, start)
	e, err2 := time.Parse(dateLayout, end)
	if err1 != nil || err2 != nil || e.Before(s) {
		return 0
	}
	return int(e.Sub(s).Hours()/24) + 1
}
