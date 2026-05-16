# Farfield

A self-owned content backend. A few small Go services — content-addressed
records, lexicon-lite schemas, image blobs, SQLite — plus the apex site at
`farfield.systems`. It is the backend for `iammatthias.com`; the website that
consumes it is a separate Astro project.

History is git's job (the authored markdown is versioned there). Farfield
holds current published state and serves it fast, with each record's CID as
its HTTP ETag.

Built on the Go standard library. Runtime dependencies, total: `modernc.org/sqlite`
(pure-Go SQLite), `golang.org/x/image` (WebP decode), `github.com/buckket/go-blurhash`,
and `gopkg.in/yaml.v3` (CLI only).

## Layout

An Andromeda-shaped workspace — `apps/` over `lib/`, composed by `deploy/`.

```
apps/content    content service — posts, open-source, recipes, art, melange, media, series
apps/feed       feed service — feed entries
apps/blobs      blob service — content-addressed blob store (local dir or R2)
apps/apex       apex site — farfield.systems, an embedded static site
apps/farfield   the `farfield` CLI

lib/core        records, canonical hashing, CIDv1
lib/store       SQLite record store
lib/schema      lexicon-lite loader + validator
lib/httpkit     shared HTTP: errors, bearer auth
lib/records     the records service engine (content + feed share it)
lib/blob        blob storage + image metadata

schemas/        lexicon-lite schema files
deploy/         Dockerfile, docker-compose.yml, .env.example
```

## Run it locally

```sh
go test ./...

FARFIELD_TOKEN=dev go run ./apps/content   # :8787
FARFIELD_TOKEN=dev go run ./apps/feed      # :8788
FARFIELD_TOKEN=dev go run ./apps/blobs     # :8789
```

Import and migrate content:

```sh
FARFIELD_TOKEN=dev go run ./apps/farfield import <markdown-dir>
FARFIELD_TOKEN=dev go run ./apps/farfield migrate-images   # ipfs:// images -> blob store
FARFIELD_TOKEN=dev go run ./apps/farfield extract-series   # runs of inline media -> series records
```

## Run it with Docker

```sh
cd deploy
cp .env.example .env        # set FARFIELD_TOKEN; set FARFIELD_BLOBS_BACKEND=r2 + R2_* to use R2
docker compose up --build
```

One image, three services, each with its own volume. The blob service uses a
local directory by default, or Cloudflare R2 when `FARFIELD_BLOBS_BACKEND=r2`.

To update a running deployment, pull the latest commit and re-run
`docker compose up -d --build` — the volumes (and their content) survive.

## Design

The full design doc lives at
`~/.gstack/projects/mission-control/iammatthias-main-design-20260515-091028.md`.
The Rust prototype this was ported from is tagged `rust-prototype`.
