package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func record(t *testing.T, jsonStr string) Record {
	t.Helper()
	var r Record
	if err := json.Unmarshal([]byte(jsonStr), &r); err != nil {
		t.Fatalf("bad test json: %v", err)
	}
	return r
}

func TestEncodingIsDeterministic(t *testing.T) {
	r := record(t, `{"title":"Pale Blue Dot","body":"# hi"}`)
	a, _ := r.CanonicalBytes()
	b, _ := r.CanonicalBytes()
	if string(a) != string(b) {
		t.Fatal("canonical bytes are not deterministic")
	}
}

func TestKeyOrderDoesNotChangeCID(t *testing.T) {
	a, err := record(t, `{"title":"x","body":"y"}`).CID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := record(t, `{"body":"y","title":"x"}`).CID()
	if a != b {
		t.Fatalf("CID changed with key order: %s != %s", a, b)
	}
}

func TestDifferentContentYieldsDifferentCID(t *testing.T) {
	a, _ := record(t, `{"body":"one"}`).CID()
	b, _ := record(t, `{"body":"two"}`).CID()
	if a == b {
		t.Fatal("different content collided to one CID")
	}
}

func TestCIDIsAWellFormedCIDv1(t *testing.T) {
	c, _ := record(t, `{"body":"hello"}`).CID()
	if !strings.HasPrefix(c, "bafkrei") {
		t.Fatalf("expected a raw-codec CIDv1 (bafkrei…), got %s", c)
	}
	if len(c) != 59 {
		t.Fatalf("expected a 59-char CIDv1, got %d chars: %s", len(c), c)
	}
}

func TestBlobCIDIsContentAddressed(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n")
	if BlobCID(png) != BlobCID(png) {
		t.Fatal("BlobCID is not stable")
	}
	if !strings.HasPrefix(BlobCID(png), "bafkrei") {
		t.Fatal("BlobCID is not a raw-codec CIDv1")
	}
}
