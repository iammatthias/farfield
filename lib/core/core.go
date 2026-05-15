// Package core defines the record type, canonical encoding, and content
// identifiers — the hashing seam every other package shares.
//
// A record is a JSON object. It is canonically encoded (Go's encoding/json
// sorts object keys) and content-hashed to a CID. The CID's one job is to be
// a stable HTTP ETag: the same record content always yields the same CID.
//
// The CID is a real CIDv1, formatted by hand — no IPLD dependency.
package core

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"strings"
)

// Record is a content record: an object of fields, one of which is
// conventionally `body` (markdown). Field types are governed by a schema in
// the schema package; core treats a record as already-canonical input.
type Record map[string]any

// CanonicalBytes returns the record's deterministic JSON encoding. Go's
// encoding/json sorts object keys, so equal records encode to equal bytes
// and therefore to an equal CID.
func (r Record) CanonicalBytes() ([]byte, error) {
	return json.Marshal(map[string]any(r))
}

// CID returns the record's content identifier.
func (r Record) CID() (string, error) {
	b, err := r.CanonicalBytes()
	if err != nil {
		return "", err
	}
	return cidV1(b), nil
}

// BlobCID returns the content identifier of a binary blob. A blob is
// self-verifying: re-hashing its bytes reproduces this CID.
func BlobCID(data []byte) string {
	return cidV1(data)
}

// cidV1 builds a CIDv1 over data: multibase-base32 of
// version(1) + codec(raw, 0x55) + multihash(sha2-256). Every field is a
// single byte, so no varint encoder is needed. Produces a real `bafkrei…`
// CID — the same shape IPFS uses for raw single-block content.
func cidV1(data []byte) string {
	digest := sha256.Sum256(data)
	buf := make([]byte, 0, 4+sha256.Size)
	// version 1, raw codec, sha2-256 code, 32-byte digest length.
	buf = append(buf, 0x01, 0x55, 0x12, 0x20)
	buf = append(buf, digest[:]...)
	// Multibase base32: lowercase RFC4648, no padding, prefix 'b'.
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	return "b" + strings.ToLower(enc)
}
