---
name: Content Addressing
description: Give records and files a content-addressed identity — a CIDv1 sha-256 hash of the content itself, computed in-process. Provides verification, change detection, ETag caching, and dedup with no IPFS node and no network — "private IPFS" in a closed system. Use when building storage for content (database records, uploaded bytes, snapshots) that should carry a verifiable, version-stamping identifier alongside its stable key.
---

# Content Addressing

## Overview

A **content identifier (CID)** is a hash of a piece of content. Identical
content always produces the same CID; any change produces a different one.
That single property buys four things, all without a network or an IPFS node:

- **Verification** — re-hash the content, compare to the CID. Tamper-evident.
- **Change detection** — the CID changed iff the content changed. Cheap diffs.
- **Caching** — hand the CID to clients as an `ETag`; they revalidate for free.
- **Dedup** — identical content has one CID, so it is stored once.

This is "private IPFS": the benefits of content addressing inside one system,
no federation, no daemon, no DHT. You are just leveraging the hash.

## The two identifiers — never conflate them

Every mutable record needs **both**, and they are different things:

| | **Key** (`slug`, `id`, `rkey`) | **CID** |
|---|---|---|
| What it is | a stable name | a hash of the content |
| Changes when content changes? | **no** | **yes** |
| You reference records by | this | never this |
| Survives an edit? | yes | no — that's the point |

A CID *must* change when content changes — that is the whole feature. A key
*must not*, or every reference and link breaks on the first edit. IPFS itself
splits these into the CID and a mutable name layer (IPNS). Your system has the
same two layers: the stable key, and the CID.

**The classic mistake:** using a CID-shaped string as a record's key. It looks
fine until the first edit, when the "key" no longer matches the content and
nothing that referenced it resolves. If you see hash-shaped values in a key
field, that is the bug.

Exception — **immutable bytes** (uploaded files, snapshots): these never
change, so the CID *is* the key. There is no separate slug, and
`GET /blobs/<cid>` is safe to cache forever (`immutable`).

## The primitive

A CIDv1 is `0x01` (v1) + `0x55` (raw codec) + `0x12 0x20` (sha2-256, 32 bytes)
+ the digest, base32-encoded, with a `b` multibase prefix. ~20 lines of Go,
standard library only:

```go
package cid

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"strings"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Of returns the CIDv1 of data — the same string format IPFS uses
// (e.g. "bafkrei…"), computed entirely in-process.
func Of(data []byte) string {
	digest := sha256.Sum256(data)
	buf := make([]byte, 0, 4+sha256.Size)
	buf = append(buf, 0x01, 0x55, 0x12, 0x20)
	buf = append(buf, digest[:]...)
	return "b" + strings.ToLower(b32.EncodeToString(buf))
}

// OfValue returns the CID of v's canonical JSON. encoding/json sorts map
// keys, so equal content yields an equal CID regardless of map order.
func OfValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return Of(b)
}
```

In the farfield monorepo this is `lib/cid` — a zero-dependency module. Bytes
go through `Of`; structured records through `OfValue`.

## What goes into the hash

Hash the **content**, nothing else. Exclude the key and the timestamps — a CID
should track what the content *is*, not its metadata. Otherwise touching
`updated_at` rewrites the CID and the "did the content change?" signal is lost.

```go
// The CID of an entry: its content, with the slug and timestamps left out.
func entryCID(e *Entry) string {
	return cid.OfValue(map[string]any{
		"collection": e.Collection,
		"title":      e.Title,
		"excerpt":    e.Excerpt,
		"body":       e.Body,
		"tags":       e.Tags,
		"published":  e.Published,
	})
}
```

Recompute the CID on **every write** (insert and update). For a database that
predates CIDs, add the column and **backfill** once on open — see the
self-migrating-sqlite skill.

## ETag — caching for free

A single-record GET sends the CID as a strong `ETag`. A client that already
holds that version sends `If-None-Match` and gets a bodyless `304`:

```go
func writeRecord(w http.ResponseWriter, r *http.Request, recCID string, v any) {
	etag := `"` + recCID + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
```

For immutable content-addressed bytes, also send
`Cache-Control: public, max-age=31536000, immutable` — the bytes for a CID can
never change.

## How far to take it — the decision tree

Three levels. Pick the smallest that does the job; do not over-build.

1. **Verifiable stamp** *(default, smallest)* — every record carries a real,
   recomputed `cid`. Used for verification and ETags. Records are fetched by
   their key; edits overwrite; no history. This is almost always enough.

2. **Addressable** — add `GET /<type>/cid/<cid>`: fetch a record by its content
   address. The CID becomes a usable address, not just a stamp. Still
   latest-only — an edit's old CID stops resolving once overwritten.

3. **Full private IPFS** — every version is written to a CID-keyed object
   store; any CID, current or historical, resolves forever; the key is a
   mutable pointer to the latest; edits create new immutable versions;
   identical content dedups. Real content addressing — and a real build. Only
   do this if version history is an actual requirement.

Most systems want level 1. Moving up is a deliberate choice, not a default.

## Anti-patterns

- **A CID in a key field.** Keys are stable; CIDs are not. The reference breaks
  on the first edit.
- **Hashing the key or timestamps.** The CID then changes on metadata-only
  writes and stops meaning "the content changed."
- **A CID-shaped string that is not a real hash.** If a value looks like a CID,
  it must *be* `cid.Of` of something current and verifiable. Decorative
  hash-shaped strings are worse than none — they imply a guarantee they don't keep.
- **Non-canonical input to the hash.** Hash sorted-key JSON (or another
  canonical form) so equal content always hashes equal. `encoding/json` sorts
  map keys; rely on that, or sort explicitly.
