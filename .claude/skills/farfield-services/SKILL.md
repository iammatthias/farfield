---
name: Farfield Services
description: Work with the farfield.systems services from any machine or session — publish feed posts, upload images to blobs, save bookmarks, create QR codes, share pastes via scrap, read content entries, and check service health. Covers base URLs, API-key auth (env keys and minted ffk_ tokens), the Cloudflare edge rules, endpoints, record shapes, and blob://​series:// body resolution. Use when reading from or writing to any *.farfield.systems API.
---

# Farfield Services — using the APIs from anywhere

## The fleet

Small single-binary Go services behind `*.farfield.systems` (homelab +
Cloudflare tunnel). They back `iammatthias.com`.

| Service | URL | Holds |
|---|---|---|
| content | `content.farfield.systems` | collections, entries, series fragments |
| feed | `feed.farfield.systems` | short ephemeral posts |
| blobs | `blobs.farfield.systems` | image/media bytes + metadata (R2) |
| bookmarks | `bookmarks.farfield.systems` | curated links w/ OG metadata |
| qr | `qr.farfield.systems` | direct + editable-proxy QR codes |
| scrap | `scrap.farfield.systems` | pastes with magic-link view tokens |
| daily | `daily.farfield.systems` | daily NASA APOD photo |
| library | `library.farfield.systems` | OPDS e-book catalog |
| sideload | `sideload.farfield.systems` | iOS build distribution → see the **farfield-sideload** skill |
| pulse | `pulse.farfield.systems` | uptime checks + traffic rollups |
| keys | `keys.farfield.systems` | mints/revokes scoped API keys (UI only) |
| apex | `farfield.systems` | landing page, not an API |

Source of truth: `docs/API.md` in the farfield repo
(`~/Developer/farfield` on this Mac). If the checkout is present, verify
against it before relying on details here.

## Edge rules (every request)

- The Cloudflare edge **403s bot User-Agents** — always send a
  real-looking UA (e.g. `curl -A "farfield-agent"`).
- Request bodies are capped around **100 MB**. Library has tus resumable
  uploads (`POST /api/upload/tus` + `PATCH` chunks) for big EPUBs; for
  anything else oversized, upload from the homelab itself.

## Auth

- **Reads** are bearer-gated per app once `<APP>_READ_KEY` is set (prod
  sets them): send `X-API-Key: <key>` or `Authorization: Bearer <key>`.
  The write key also reads (and additionally previews drafts).
- **Writes** need the app's write key (`<APP>_API_KEY`), same headers.
- **Minted keys**: the keys app issues revocable `ffk_…` tokens (scope
  read / upload / write, per app or `*`, optional expiry) that every app
  honors at the same gates. Prefer these for agents and third parties —
  they revoke without a redeploy. Minting is a password-gated UI; ask the
  user to mint one.
- **Still public, no key**: blob bytes `/blobs/{cid}` (+ `/meta`), QR
  image/redirect `/qr/{id}` + `/r/{id}`, daily, every `/status`, and the
  rate-limited single-record reads `feed /api/posts/{slug}` and
  `content /api/entries/{slug}` (a draft is a `404` without the write key).
  Enumerating lists stay gated.

Getting keys on this machine: dev repo `.env` (`~/Developer/farfield/.env`),
production `.env` on the homelab (`ssh iam@homelab.local`,
`~/projects/farfield/.env`), or a minted `ffk_` key. Load into a shell var
without echoing the value:
`KEY=$(grep '^FEED_API_KEY=' path/to/.env | cut -d= -f2-)`.

## Conventions

- Every record has a stable **key** (its `slug`/`id`) and a **CID** — a
  CIDv1 sha-256 of its content. Single-record GETs send the CID as a
  strong `ETag`; use `If-None-Match: "<cid>"` for `304`s. Blob bytes are
  immutable — cache forever.
- Timestamps are RFC3339 UTC.
- Read APIs return published records only.

## Endpoints

**content** — `GET /api/collections` · `GET /api/entries[?collection=slug]`
· `GET /api/entries/{slug}` (public, rate-limited) · `GET /api/series` ·
`GET /api/series/{slug}` · `POST /api/entries` · `PUT|DELETE
/api/entries/{slug}` · `POST /api/series` (always assigns a fresh slug).

**feed** — `GET /api/posts` · `GET /api/posts/{slug}` (public,
rate-limited) · `POST /api/posts` · `PUT|DELETE /api/posts/{slug}`.

**blobs** — `GET /blobs/{cid}` (raw bytes, immutable) · `GET
/blobs/{cid}/meta` · `GET /blobs[?page=N]` (gated) · `POST /blobs` (bytes →
`BlobMeta`) · `DELETE /blobs/{cid}`.

**bookmarks** — `GET /api/bookmarks` (public-flagged only) · `GET
/api/bookmarks/{id}` · `POST /api/bookmarks` (server fetches OG metadata on
save) · `PUT|DELETE /api/bookmarks/{id}`.

**qr** — `GET /qr/{id}` (SVG) · `GET /r/{id}` (`303` redirect, proxy mode)
· `GET /api/codes[/{id}]` · `POST /api/codes` · `PUT|DELETE
/api/codes/{id}`. `direct` mode encodes the target string verbatim; `proxy`
mode encodes `/r/{id}` so the destination stays editable after printing.
Image/redirect only work for records both `public` and `enabled`.

**scrap** — `POST /api/pastes?title=&lang=&visibility=&expires=&token=`
(body = raw paste text; returns the paste URL as plain text;
`token=generate` mints a magic view token and returns it on line 2;
`token=<secret>` sets one) · `DELETE /api/pastes/{id}` · `POST
/api/pastes/{id}/token/roll` · `DELETE /api/pastes/{id}/token`. Public
reads: `GET /{id}`, `GET /{id}/raw`, `GET /pastes` (public index).
`visibility`: `public` | `unlisted` (default) | `private` (session-only —
not for sharing). `expires`: `never` | `1h` | `1d` | `1w` | `1m` — anything
else is an error, not "forever". A token forces visibility down to at
least unlisted.

**daily** — `GET /api/photo` (today) · `GET /api/photo/{date}` · `GET
/api/photos[?page=N]`. HTML at `/photo`, `/photo/{date}`,
`/photo/archive`. All public.

**library** — OPDS under `/opds` (catalog auth); `POST /api/books`
(upload-scoped key) · `PUT /api/books/{cid}/collection` · `DELETE
/api/books/{cid}` (write key) · tus at `/api/upload/tus`.

**health** — every service: `GET /status` →
`{ "service", "ok", … }`, public. Sweep the fleet by curling each; pulse
aggregates uptime and per-app traffic.

## Record shapes

```jsonc
// Entry    { "collection", "slug", "cid", "title", "excerpt"?, "body",
//            "tags": [], "published", "publishedAt"?, "createdAt", "updatedAt" }
// Series   { "slug", "cid", "title"?, "body", "createdAt", "updatedAt" }
// Post     { "slug", "cid", "body", "tags": [], "createdAt", "updatedAt" }
// BlobMeta { "cid", "size", "mime", "width"?, "height"?, "blurhash"?, "dominantColor"? }
// Bookmark { "id", "url", "title", "description", "category", "public", "cid",
//            "og*"?, "favicon"?, "createdAt", "updatedAt" }
// QRCode   { "id", "label", "mode", "target", "ec", "public", "enabled", "cid", … }
```

## Body URIs

Entry and post markdown embeds two custom URIs — resolve before rendering:

- `blob://<cid>` → `https://blobs.farfield.systems/blobs/<cid>` (read
  `/meta` for width/height/blurhash).
- `![](series://<slug>)` → fetch `GET /api/series/<slug>` and splice the
  fragment's `body` in place of the whole image construct.

**Never rewrite these URIs when writing bodies back** — store `blob://` /
`series://` forms, not resolved URLs.

## Recipes

**Post to the feed with an image** — upload bytes first, embed the CID:

```sh
CID=$(curl -sS -A "farfield-agent" -H "X-API-Key: $BLOBS_KEY" \
  --data-binary @photo.jpg -H "Content-Type: image/jpeg" \
  https://blobs.farfield.systems/blobs | jq -r .cid)
curl -sS -A "farfield-agent" -H "X-API-Key: $FEED_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"body\": \"morning light\\n\\n![](blob://$CID)\", \"tags\": [\"photo\"]}" \
  https://feed.farfield.systems/api/posts
```

**Save a bookmark** (server enriches with OG metadata):

```sh
curl -sS -A "farfield-agent" -H "X-API-Key: $BM_KEY" \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/post", "category": "reading", "public": true}' \
  https://bookmarks.farfield.systems/api/bookmarks
```

**Share a paste with a magic link**:

```sh
curl -sS -A "farfield-agent" -H "X-API-Key: $SCRAP_KEY" \
  --data-binary @notes.md \
  "https://scrap.farfield.systems/api/pastes?title=notes&visibility=unlisted&expires=1d&token=generate"
```

**Editable QR** (proxy mode — reprint-proof):

```sh
curl -sS -A "farfield-agent" -H "X-API-Key: $QR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"label": "menu", "mode": "proxy", "target": "https://example.com/menu", "public": true, "enabled": true}' \
  https://qr.farfield.systems/api/codes
```
