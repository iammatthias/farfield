package main

import "github.com/iammatthias/farfield/lib/cid"

// BlobCID returns the content identifier of a blob — a CIDv1 over its bytes.
// A blob is self-verifying: re-hashing the bytes reproduces this CID.
func BlobCID(data []byte) string { return cid.Of(data) }

// validCID reports whether s is well-formed — only the lowercase base32
// alphabet — so a CID can never be a path-traversal payload.
func validCID(s string) bool {
	if len(s) < 2 || len(s) > 80 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
