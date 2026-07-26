package store

import (
	"math"
	"strings"
	"testing"
)

func TestShortIDShape(t *testing.T) {
	for range 200 {
		id := ShortID()
		if len(id) != idLength {
			t.Fatalf("ShortID() = %q, want %d characters", id, idLength)
		}
		for _, c := range id {
			if !strings.ContainsRune(idAlphabet, c) {
				t.Fatalf("ShortID() = %q contains %q, outside the alphabet", id, c)
			}
		}
	}
}

// A plain byte modulo would make the first 256 % 36 = 4 characters of the
// alphabet about 14% more likely than the rest. Rejection sampling removes
// that skew; this checks the distribution is flat enough to prove it.
func TestShortIDIsUnbiased(t *testing.T) {
	const samples = 40000
	counts := make(map[rune]int, len(idAlphabet))
	for range samples {
		for _, c := range ShortID() {
			counts[c]++
		}
	}
	if len(counts) != len(idAlphabet) {
		t.Fatalf("saw %d distinct characters, want all %d", len(counts), len(idAlphabet))
	}

	total := samples * idLength
	expected := float64(total) / float64(len(idAlphabet))
	// The old modulo bias was ~14%; anything within 6% of uniform across this
	// many draws is comfortably flat and comfortably clear of that.
	const tolerance = 0.06
	for c, n := range counts {
		if drift := math.Abs(float64(n)-expected) / expected; drift > tolerance {
			t.Errorf("character %q appeared %d times, %.1f%% off uniform (%.0f)",
				c, n, drift*100, expected)
		}
	}
}

func TestShortIDIsDistinct(t *testing.T) {
	seen := make(map[string]bool, 5000)
	for range 5000 {
		id := ShortID()
		if seen[id] {
			t.Fatalf("ShortID() returned %q twice in 5000 draws", id)
		}
		seen[id] = true
	}
}
