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

func TestKeepStamp(t *testing.T) {
	cases := []struct{ slug, current, want string }{
		{"buddydex", "1789660275538-buddydex", "1789660275538-buddydex"},          // bare replacement inherits the stamp
		{"new-name", "1789660275538-buddydex", "1789660275538-new-name"},          // a deliberate rename keeps the key's stamp
		{"1789660275538-x", "1789660275538-buddydex", "1789660275538-x"},          // already stamped: verbatim
		{"1700000000000-x", "1789660275538-buddydex", "1700000000000-x"},          // a different stamp is the caller's call
		{"buddydex", "buddydex", "buddydex"},                                      // nothing to preserve
		{"", "1789660275538-buddydex", ""},                                        // empty stays empty for validation
		{"100-days-of-code", "1789660275538-x", "1789660275538-100-days-of-code"}, // a short numeric run is not a stamp
	}
	for _, c := range cases {
		if got := keepStamp(c.slug, c.current); got != c.want {
			t.Errorf("keepStamp(%q, %q) = %q, want %q", c.slug, c.current, got, c.want)
		}
	}
}

// TestUpdateKeepsSlugStamp is the editor's second save: the create stamped
// the derived slug, the slug field is still blank, so the update arrives
// with a bare title-derived slug. The stored key must not lose its prefix.
func TestUpdateKeepsSlugStamp(t *testing.T) {
	db := openTestDB(t)
	e := &Entry{Collection: "blog", Slug: slugify("Buddydex"), Title: "Buddydex", Body: "x"}
	if err := insertEntry(db, e); err != nil {
		t.Fatal(err)
	}
	stamped := e.Slug
	if !stampedSlug.MatchString(stamped) {
		t.Fatalf("insert did not stamp: %q", stamped)
	}
	again := &Entry{Collection: "blog", Slug: slugify("Buddydex"), Title: "Buddydex", Body: "xy"}
	if err := updateEntry(db, stamped, again); err != nil {
		t.Fatal(err)
	}
	if again.Slug != stamped {
		t.Fatalf("update renamed %q to %q", stamped, again.Slug)
	}
	got, err := getEntry(db, stamped)
	if err != nil || got == nil {
		t.Fatalf("entry gone from its stamped key: %v, %v", got, err)
	}
	if got.Body != "xy" || got.CID != again.CID {
		t.Fatalf("stored body/cid %q/%q, want %q/%q", got.Body, got.CID, "xy", again.CID)
	}
	if bare, _ := getEntry(db, "buddydex"); bare != nil {
		t.Fatalf("bare slug %q resolves; the key was rewritten", "buddydex")
	}
}
