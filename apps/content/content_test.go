package main

import "testing"

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
