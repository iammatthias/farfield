package main

import "testing"

func TestSplitTags(t *testing.T) {
	got := splitTags("life, web ,, life , go ")
	want := []string{"life", "web", "go"}
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
		t.Errorf("nil round-trip = %v, want empty", got)
	}
	r := decodeTags(encodeTags([]string{"x", "y"}))
	if len(r) != 2 || r[0] != "x" || r[1] != "y" {
		t.Errorf("round-trip = %v, want [x y]", r)
	}
}

func TestSplitFrontmatter(t *testing.T) {
	front, body, err := splitFrontmatter("---\ncreated: \"2026-01-01T00:00:00Z\"\n---\nhello there")
	if err != nil {
		t.Fatalf("splitFrontmatter: %v", err)
	}
	if body != "hello there" {
		t.Errorf("body = %q, want %q", body, "hello there")
	}
	if front == "" {
		t.Error("front is empty")
	}
}
