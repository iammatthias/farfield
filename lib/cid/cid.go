// Package cid computes content identifiers — CIDv1 (raw codec, sha2-256,
// multibase-base32, 'b'-prefixed). farfield uses them for inherent versioning
// and verifiability: re-hashing the same content always reproduces its CID,
// for blob bytes and markdown records alike. Standard library only.
package cid

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"io"
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

// OfReader hashes r in a single streaming pass and returns its CIDv1 along
// with the number of bytes read. Use it for content too large to buffer —
// file uploads, database snapshots — where Of would mean holding the whole
// payload in memory.
func OfReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	buf := make([]byte, 0, 4+sha256.Size)
	buf = append(buf, 0x01, 0x55, 0x12, 0x20) // v1, raw codec, sha2-256, 32 bytes
	buf = append(buf, h.Sum(nil)...)
	return "b" + strings.ToLower(b32.EncodeToString(buf)), n, nil
}

// Valid reports whether s is a well-formed farfield CID: the 'b' multibase
// prefix, base32-decodable, and carrying the v1/raw/sha2-256/32-byte header.
func Valid(s string) bool {
	if len(s) != 59 || s[0] != 'b' {
		return false
	}
	raw, err := b32.DecodeString(strings.ToUpper(s[1:]))
	if err != nil || len(raw) != 4+sha256.Size {
		return false
	}
	return raw[0] == 0x01 && raw[1] == 0x55 && raw[2] == 0x12 && raw[3] == 0x20
}
