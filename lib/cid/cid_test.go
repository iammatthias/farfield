package cid

import (
	"strings"
	"testing"
)

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

func TestOfReaderMatchesOf(t *testing.T) {
	data := []byte("farfield streaming hash")
	got, n, err := OfReader(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("OfReader: %v", err)
	}
	if want := Of(data); got != want {
		t.Fatalf("OfReader = %s, want %s", got, want)
	}
	if n != int64(len(data)) {
		t.Fatalf("OfReader read %d bytes, want %d", n, len(data))
	}
}

func TestValid(t *testing.T) {
	good := Of([]byte("anything"))
	if !Valid(good) {
		t.Fatalf("Valid rejected a real CID: %s", good)
	}
	for _, bad := range []string{
		"",
		"b",
		good[1:],                      // missing multibase prefix
		"x" + good[1:],                // wrong prefix
		good + "a",                    // wrong length
		good[:len(good)-1] + "!",      // non-base32 byte
		"b" + strings.Repeat("a", 58), // right shape, wrong header
	} {
		if Valid(bad) {
			t.Fatalf("Valid accepted %q", bad)
		}
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
