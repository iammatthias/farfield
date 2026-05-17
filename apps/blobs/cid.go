package main

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// BlobCID returns the content identifier of a blob: a real CIDv1 —
// multibase-base32 of version(1) + raw codec(0x55) + sha2-256 multihash.
// A blob is self-verifying: re-hashing its bytes reproduces this CID.
func BlobCID(data []byte) string {
	digest := sha256.Sum256(data)
	buf := make([]byte, 0, 4+sha256.Size)
	// version 1, raw codec, sha2-256 code, 32-byte digest length.
	buf = append(buf, 0x01, 0x55, 0x12, 0x20)
	buf = append(buf, digest[:]...)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	return "b" + strings.ToLower(enc)
}

// validCID reports whether cid is well-formed — only the lowercase base32
// alphabet — so a CID can never be a path-traversal payload.
func validCID(cid string) bool {
	if len(cid) < 2 || len(cid) > 80 {
		return false
	}
	for _, c := range cid {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
