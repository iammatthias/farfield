package main

import "testing"

func TestBlobCIDDeterministic(t *testing.T) {
	a := BlobCID([]byte("hello farfield"))
	b := BlobCID([]byte("hello farfield"))
	if a != b {
		t.Fatalf("BlobCID not deterministic: %s != %s", a, b)
	}
	if !validCID(a) {
		t.Fatalf("BlobCID produced an invalid cid: %s", a)
	}
}

func TestBlobCIDDistinct(t *testing.T) {
	if BlobCID([]byte("one")) == BlobCID([]byte("two")) {
		t.Fatal("distinct inputs produced the same cid")
	}
}

func TestValidCIDRejectsBadInput(t *testing.T) {
	bad := []string{"", "a", "../etc", "ABC", "has/slash", "has.dot", "UPPER"}
	for _, c := range bad {
		if validCID(c) {
			t.Errorf("validCID accepted bad cid: %q", c)
		}
	}
}

func TestDeriveMetadataNonImage(t *testing.T) {
	m, err := DeriveMetadata([]byte("not an image, just some bytes"))
	if err != nil {
		t.Fatalf("DeriveMetadata: %v", err)
	}
	if m.CID == "" || m.Size == 0 || m.Mime == "" {
		t.Fatalf("DeriveMetadata returned incomplete meta: %+v", m)
	}
	if m.Width != 0 || m.Height != 0 {
		t.Errorf("non-image got dimensions: %dx%d", m.Width, m.Height)
	}
}
