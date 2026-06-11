package main

import (
	"crypto/sha256"
	"math/rand/v2"
	"time"
)

// The daily generative artifacts are pure functions of the calendar date.
// Each derives everything it needs from a domain-separated seed — nothing
// about an artifact is ever stored, so any date renders identically forever.

// Artifact seed domains. Each artifact hashes its own domain so sibling
// artifacts never share a random stream on the same date.
const (
	domainArt = "art"
)

// artifactEpoch is day zero of the generative artifacts. Day indices count
// forward from here; dates before it do not exist.
const artifactEpoch = "2020-01-01"

// seed derives the 32-byte deterministic seed for one artifact-day:
// sha256(domain + ":" + date). The domain prefix separates artifacts —
// seed("art", d) and seed("sudoku", d) share nothing.
func seed(domain, dateISO string) [32]byte {
	return sha256.Sum256([]byte(domain + ":" + dateISO))
}

// newRNG returns the deterministic PRNG for one artifact-day — ChaCha8 keyed
// by the full 32-byte seed, so the whole hash feeds the stream. Stdlib only.
func newRNG(domain, dateISO string) *rand.ChaCha8 {
	s := seed(domain, dateISO)
	return rand.NewChaCha8(s)
}

// randFloat returns the stream's next value in [0, 1), built from the top 53
// bits of a draw so the float is exactly representable.
func randFloat(r *rand.ChaCha8) float64 {
	return float64(r.Uint64()>>11) / (1 << 53)
}

// dayIndex returns the number of days from artifactEpoch to date — negative
// for pre-epoch dates, which callers treat as nonexistent.
func dayIndex(dateISO string) (int64, error) {
	d, err := time.Parse(dateLayout, dateISO)
	if err != nil {
		return 0, err
	}
	e, _ := time.Parse(dateLayout, artifactEpoch)
	return int64(d.Sub(e) / (24 * time.Hour)), nil
}

// addDays returns the date n days after (or before, negative) date.
func addDays(date string, n int) string {
	d, err := time.Parse(dateLayout, date)
	if err != nil {
		return ""
	}
	return d.AddDate(0, 0, n).Format(dateLayout)
}
