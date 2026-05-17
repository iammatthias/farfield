package cid

import "testing"

func TestOfDeterministic(t *testing.T) {
	a := Of([]byte("farfield"))
	if a != Of([]byte("farfield")) {
		t.Fatal("Of is not deterministic")
	}
	if Of([]byte("a")) == Of([]byte("b")) {
		t.Fatal("distinct inputs produced the same CID")
	}
	if len(a) < 2 || a[0] != 'b' {
		t.Fatalf("malformed CID: %q", a)
	}
}

func TestOfValueCanonical(t *testing.T) {
	// Equal content supplied in a different key order must yield an equal CID.
	x := OfValue(map[string]any{"a": 1, "b": 2})
	y := OfValue(map[string]any{"b": 2, "a": 1})
	if x != y {
		t.Fatalf("OfValue is not canonical: %s != %s", x, y)
	}
	if OfValue(map[string]any{"a": 1}) == OfValue(map[string]any{"a": 2}) {
		t.Fatal("distinct content produced the same CID")
	}
}
