package main

import (
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Source identifiers. nasa is the default; ufo is the easter-egg source.
const (
	sourceNASA = "nasa"
	sourceUFO  = "ufo"
)

// userAgent identifies the calendar service to the upstreams it polls.
const userAgent = "farfield-calendar/1.0 (+https://farfield.systems)"

// Resilience tuning — these keep the service from hammering rate-limited
// upstreams. Cache-first reads mean upstreams are touched only on a miss; on
// top of that a failure trips a cooldown and a negative cache.
const (
	nasaCooldown      = 10 * time.Minute // pause upstream calls after a failure
	negativeTTL       = 2 * time.Hour    // remember a failed date for this long
	ufoScrapeInterval = 12 * time.Hour   // minimum gap between UFO scrapes
)

// SourceInfo describes a selectable photo source for the API and the UI.
type SourceInfo struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// sources is the source registry, in display order.
var sources = []SourceInfo{
	{
		Name:        sourceNASA,
		Label:       "NASA · Astronomy Picture of the Day",
		Description: "A new astronomy image every day since 1995, from NASA's APOD.",
	},
	{
		Name:        sourceUFO,
		Label:       "Dept. of War · UFO Imagery",
		Description: "Declassified UAP/UFO imagery published at war.gov/UFO.",
	},
}

// canonicalSource normalises a requested source name to a known identifier,
// defaulting to NASA. It accepts a few friendly aliases.
func canonicalSource(s string) string {
	switch s {
	case sourceUFO, "war", "uap", "dod":
		return sourceUFO
	case sourceNASA, "apod", "":
		return sourceNASA
	default:
		return sourceNASA
	}
}

// sourceInfo returns the registry entry for a source name.
func sourceInfo(name string) SourceInfo {
	for _, s := range sources {
		if s.Name == name {
			return s
		}
	}
	return sources[0]
}

// otherSource returns the source to switch to — the basis of the UI easter egg.
func otherSource(name string) string {
	if name == sourceUFO {
		return sourceNASA
	}
	return sourceUFO
}

// fetcher performs upstream HTTP calls and tracks the state that keeps the
// service resilient: a cooldown after failures and a per-date negative cache.
type fetcher struct {
	client  *http.Client
	nasaKey string

	mu             sync.Mutex
	nasaCooldownAt time.Time            // upstream NASA calls paused until here
	negative       map[string]time.Time // "source:date" -> when the fetch failed
	lastUFOAttempt time.Time            // when the UFO page was last scraped
}

// newFetcher builds a fetcher with a bounded HTTP timeout.
func newFetcher(nasaKey string) *fetcher {
	return &fetcher{
		client:   &http.Client{Timeout: 20 * time.Second},
		nasaKey:  nasaKey,
		negative: map[string]time.Time{},
	}
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

// markNegative records that a specific date failed, so it is not retried on
// every request for the next negativeTTL.
func (f *fetcher) markNegative(source, date string) {
	f.mu.Lock()
	f.negative[source+":"+date] = time.Now()
	f.mu.Unlock()
}

// negativeHit reports whether a date failed recently enough to skip retrying.
func (f *fetcher) negativeHit(source, date string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.negative[source+":"+date]
	return ok && time.Since(at) < negativeTTL
}

// ── orchestration: cache-first reads with resilient upstream fallback ───────

// photoForDate returns the photo for one (source, date), fetching it from the
// upstream only on a cache miss. A failed fetch returns (nil, nil) — the
// caller renders a not-found rather than an error.
func (s *Server) photoForDate(source, date string) (*Photo, error) {
	p, err := getPhoto(s.db, source, date)
	if err != nil || p != nil {
		return p, err
	}
	switch source {
	case sourceNASA:
		return s.nasaEnsureDay(date)
	case sourceUFO:
		if err := s.ufoEnsure(); err != nil {
			slog.Warn("ufo ensure failed", "err", err)
		}
		return getPhoto(s.db, sourceUFO, date)
	}
	return nil, nil
}

// todayPhoto returns the most recent photo for a source. For NASA it walks
// back a few days, since the current day's APOD is sometimes posted late.
func (s *Server) todayPhoto(source string) (*Photo, string, error) {
	switch source {
	case sourceUFO:
		if err := s.ufoEnsure(); err != nil {
			slog.Warn("ufo ensure failed", "err", err)
		}
		p, err := latestPhoto(s.db, sourceUFO)
		if err != nil || p == nil {
			return nil, todayUTC(), err
		}
		return p, p.Date, nil
	default:
		now := time.Now().UTC()
		for back := 0; back < 4; back++ {
			date := now.AddDate(0, 0, -back).Format(dateLayout)
			p, err := s.photoForDate(sourceNASA, date)
			if err != nil {
				return nil, date, err
			}
			if p != nil {
				return p, date, nil
			}
		}
		// Nothing fresh upstream — fall back to the newest cached day.
		p, err := latestPhoto(s.db, sourceNASA)
		if err != nil || p == nil {
			return nil, todayUTC(), err
		}
		return p, p.Date, nil
	}
}

// nasaEnsureDay fetches and caches one NASA day, honouring the cooldown and
// negative cache. A miss or a failure yields (nil, nil).
func (s *Server) nasaEnsureDay(date string) (*Photo, error) {
	if !nasaDateInRange(date) || s.fetcher.negativeHit(sourceNASA, date) {
		return nil, nil
	}
	if !s.fetcher.nasaAllowed() {
		return nil, nil // cooling down — serve cache only
	}
	p, err := s.fetcher.nasaDay(date)
	if err != nil {
		slog.Warn("nasa day fetch failed", "date", date, "err", err)
		s.fetcher.markNegative(sourceNASA, date)
		s.fetcher.noteNASAError()
		return nil, nil
	}
	if err := upsertPhoto(s.db, p); err != nil {
		return nil, err
	}
	return getPhoto(s.db, sourceNASA, date)
}

// nasaEnsureRange warms the cache for a date range in a single upstream call,
// but only when the range is not already fully cached.
func (s *Server) nasaEnsureRange(start, end string) error {
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
	photos, err := s.fetcher.nasaRange(start, end)
	if err != nil {
		slog.Warn("nasa range fetch failed", "start", start, "end", end, "err", err)
		s.fetcher.noteNASAError()
		return nil
	}
	for i := range photos {
		if err := upsertPhoto(s.db, &photos[i]); err != nil {
			return err
		}
	}
	return nil
}

// ufoEnsure makes sure the UFO source has cached entries, scraping war.gov at
// most once per ufoScrapeInterval. A failed scrape with an empty cache seeds
// placeholder items; a failed scrape with a warm cache leaves it untouched.
func (s *Server) ufoEnsure() error {
	n, err := countPhotos(s.db, sourceUFO)
	if err != nil {
		return err
	}
	s.fetcher.mu.Lock()
	recent := !s.fetcher.lastUFOAttempt.IsZero() &&
		time.Since(s.fetcher.lastUFOAttempt) < ufoScrapeInterval
	s.fetcher.mu.Unlock()
	if n >= daysBetween(calendarStart, todayUTC()) && recent {
		return nil // cache is warm enough
	}

	s.fetcher.mu.Lock()
	s.fetcher.lastUFOAttempt = time.Now()
	s.fetcher.mu.Unlock()

	photos, err := s.fetcher.ufoScrape()
	if err != nil {
		slog.Warn("ufo scrape failed", "err", err)
		if n > 0 {
			return nil // keep the existing cache
		}
		return s.storeUFO(ufoPlaceholders(), false)
	}
	slog.Info("ufo scrape ok", "items", len(photos))
	return s.storeUFO(photos, true)
}

// storeUFO writes scraped UFO items, assigning each a synthetic date counting
// back from today to calendarStart so they form a consecutive calendar. The
// upstream release is a finite set, so if the calendar eventually grows past the
// release length, entries repeat rather than leaving future days empty. A real
// scrape replaces the whole source; a placeholder seed only inserts.
func (s *Server) storeUFO(photos []Photo, replace bool) error {
	if len(photos) == 0 {
		return nil
	}
	if replace {
		if _, err := s.db.Exec(`DELETE FROM photos WHERE source = ?`, sourceUFO); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	want := daysBetween(calendarStart, todayUTC())
	for i := 0; i < want; i++ {
		p := photos[i%len(photos)]
		p.Source = sourceUFO
		p.Date = now.AddDate(0, 0, -i).Format(dateLayout)
		if err := upsertPhoto(s.db, &p); err != nil {
			return err
		}
	}
	return nil
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
