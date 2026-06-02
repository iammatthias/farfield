package main

import (
	"fmt"
	"testing"
	"time"
)

func TestStampSlug(t *testing.T) {
	at := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	ms := at.UnixMilli()

	// A bare slug gets the millisecond-epoch prefix.
	if got, want := stampSlug("farfield", at), fmt.Sprintf("%d-farfield", ms); got != want {
		t.Errorf("stampSlug(bare) = %q, want %q", got, want)
	}
	// Idempotent: an already-stamped slug is left untouched.
	already := fmt.Sprintf("%d-farfield", ms)
	if got := stampSlug(already, at); got != already {
		t.Errorf("stampSlug(stamped) = %q, want unchanged %q", got, already)
	}
	// A numeric-leading title is not mistaken for a stamp.
	if got, want := stampSlug("100-days-of-code", at), fmt.Sprintf("%d-100-days-of-code", ms); got != want {
		t.Errorf("stampSlug(numeric title) = %q, want %q", got, want)
	}
	// An empty slug stays empty (callers validate non-empty separately).
	if got := stampSlug("", at); got != "" {
		t.Errorf("stampSlug(empty) = %q, want empty", got)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Hello World":        "hello-world",
		"  Trim  Me  ":       "trim-me",
		"Already-slug":       "already-slug",
		"Lots!!!Of###Punct.": "lots-of-punct",
		"":                   "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitTags(t *testing.T) {
	got := splitTags("go, sqlite ,, go , web ")
	want := []string{"go", "sqlite", "web"}
	if len(got) != len(want) {
		t.Fatalf("splitTags = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tag %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEncodeDecodeTags(t *testing.T) {
	if got := decodeTags(encodeTags(nil)); len(got) != 0 {
		t.Errorf("round-trip of nil = %v, want empty", got)
	}
	round := decodeTags(encodeTags([]string{"a", "b"}))
	if len(round) != 2 || round[0] != "a" || round[1] != "b" {
		t.Errorf("round-trip = %v, want [a b]", round)
	}
}
