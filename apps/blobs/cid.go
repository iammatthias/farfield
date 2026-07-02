package main

import "github.com/iammatthias/farfield/lib/cid"

// BlobCID returns the content identifier of a blob — a CIDv1 over its bytes.
// A blob is self-verifying: re-hashing the bytes reproduces this CID.
func BlobCID(data []byte) string { return cid.Of(data) }

// validCID reports whether s is well-formed — only the lowercase base32
// alphabet — so a CID can never be a path-traversal payload.
func validCID(s string) bool { return cid.WellFormed(s) }
