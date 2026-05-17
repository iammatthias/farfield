// Package cid computes content identifiers — CIDv1 (raw codec, sha2-256,
// multibase-base32, 'b'-prefixed). farfield uses them for inherent versioning
// and verifiability: re-hashing the same content always reproduces its CID,
// for blob bytes and markdown records alike. Standard library only.
package cid

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"strings"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Of returns the CIDv1 of raw bytes.
func Of(data []byte) string {
	digest := sha256.Sum256(data)
	buf := make([]byte, 0, 4+sha256.Size)
	buf = append(buf, 0x01, 0x55, 0x12, 0x20) // v1, raw codec, sha2-256, 32 bytes
	buf = append(buf, digest[:]...)
	return "b" + strings.ToLower(b32.EncodeToString(buf))
}

// OfValue returns the CIDv1 of v's canonical JSON encoding. encoding/json
// sorts object keys, so equal content yields an equal CID regardless of the
// order fields are supplied in.
func OfValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return Of(b)
}
