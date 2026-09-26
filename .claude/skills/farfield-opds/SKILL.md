---
name: farfield-opds
description: Upload EPUBs to the farfield OPDS library (library.farfield.systems) and file them into collections — small books in one POST, large ones via tus resumable chunks. Covers the upload-scoped key (LIBRARY_KEY), the Cloudflare edge rules, the response shape, and how to verify a book landed. Use when asked to upload, publish, shelve, or add an EPUB/e-book to the library or OPDS catalog.
---

# Farfield OPDS — putting EPUBs in the library

`library.farfield.systems` is an OPDS e-book catalog. Books are keyed by a
**CID** (CIDv1 sha-256 of the EPUB bytes): the same file uploaded twice is the
same book, and a re-upload keeps its original collection.

## Key

Expect an **upload-scoped** minted token (`ffk_…`) in `LIBRARY_KEY`
(`~/.config/farfield/library.env`, `chmod 600`). Load it without echoing:

```sh
set -a; . ~/.config/farfield/library.env; set +a
```

Upload scope can: `POST /api/books`, tus upload, `PUT /api/books/{cid}/collection`.
It **cannot** read the catalog (`/opds/*` → 401) or delete books — that is by
design. Never print, log, or commit the key. If it 401s, ask the user for a
fresh one (they revoke/mint at keys.farfield.systems).

## Edge rules

- Always send a real User-Agent (`-A "farfield-agent"`) — the Cloudflare edge
  403s bot UAs.
- Request bodies cap near **100 MB**. Under ~90 MB: single POST. Over: tus.

## Upload (≤ ~90 MB)

```sh
F=path/to/book.epub
curl -sS -A "farfield-agent" -H "Authorization: Bearer $LIBRARY_KEY" \
  -H "Content-Type: application/epub+zip" --data-binary @"$F" \
  "https://library.farfield.systems/api/books?filename=$(basename "$F")&collection=<optional>"
```

`201` → the book as JSON (`cid`, `title`, `author`, `collection`, …).
`400` means the file isn't a valid EPUB — check it with `unzip -l` (needs a
`mimetype` entry and an OPF). `filename` is only a title fallback;
`collection` is optional (omit = uncategorized).

## Upload (> ~90 MB) — tus

```sh
F=big.epub; LEN=$(wc -c <"$F" | tr -d ' '); CH=$((50*1024*1024))
B="https://library.farfield.systems"
META="filename $(printf %s "$(basename "$F")" | base64)"   # add ",collection <b64>" if wanted
LOC=$(curl -sS -A "farfield-agent" -H "Authorization: Bearer $LIBRARY_KEY" \
  -H "Tus-Resumable: 1.0.0" -H "Upload-Length: $LEN" -H "Upload-Metadata: $META" \
  -X POST -D - -o /dev/null "$B/api/upload/tus" | awk -F': ' 'tolower($1)=="location"{print $2}' | tr -d '\r')
case "$LOC" in http*) ;; *) LOC="$B$LOC";; esac
OFF=0
while [ "$OFF" -lt "$LEN" ]; do
  tail -c +$((OFF+1)) "$F" | head -c "$CH" > /tmp/tus.chunk
  OFF=$(curl -sS -A "farfield-agent" -H "Authorization: Bearer $LIBRARY_KEY" \
    -H "Tus-Resumable: 1.0.0" -H "Upload-Offset: $OFF" \
    -H "Content-Type: application/offset+octet-stream" \
    --data-binary @/tmp/tus.chunk -X PATCH -D - -o /dev/null "$LOC" \
    | awk -F': ' 'tolower($1)=="upload-offset"{print $2}' | tr -d '\r')
done
rm -f /tmp/tus.chunk
```

Finalizing (unzip, metadata, cover) runs in the background after the last
PATCH, so the cid is not in that response. Poll `HEAD $LOC` every few
seconds until `X-Library-Status: done` — then `X-Library-Cid` holds the
book's cid (a 200 MB book took ~40 s). `X-Library-Status: error` comes with
the reason in `X-Library-Error`. If a chunk fails mid-upload, `HEAD $LOC`
also returns the server's `Upload-Offset` — resume from there.

## Organize

```sh
curl -sS -A "farfield-agent" -H "Authorization: Bearer $LIBRARY_KEY" -X PUT \
  "https://library.farfield.systems/api/books/<cid>/collection?collection=<name>"
```

Empty `collection=` clears it. Collections are implicit — one exists when a
book names it.

## Verify

`curl -sS -A "farfield-agent" https://library.farfield.systems/status` is
public and returns the book count — it should go up by one (unless the CID
already existed). Report the returned `cid` and `title` to the user. The book
then shows in any OPDS reader pointed at `https://library.farfield.systems/opds`
(reader credentials are separate from the upload key).
