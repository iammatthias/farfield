# Farfield

A self-owned content backend. One Rust service: content-addressed records,
lexicon-lite schemas, image blobs in R2, SQLite. It is the backend for
`iammatthias.com` — not the website and not the landing page, which are
separate Astro projects.

History is git's job (the authored markdown is versioned there). Farfield
holds current published state and serves it fast, with each record's CID as
its HTTP ETag.

## Layout

An Andromeda-shaped workspace: shared library crates plus three deployable
app services. `content` and `feed` are typed-records services — each its own
SQLite DB and domain — built on a shared records engine. `blobs` is a shared
content-addressed blob service the others consume.

```
crates/core      record types, canonical encoding, CID
crates/schema    lexicon-lite loader + validator + TS codegen
crates/store     SQLite record store (rusqlite)
crates/httpkit   shared HTTP scaffolding: auth, socket split, errors, status
crates/records   the records service engine (store + schema + httpkit)
crates/blobs     blob storage: R2 + local-dir backends, GC
crates/snapshot  VACUUM INTO + R2 upload/restore + retention

apps/content     content.farfield.systems — post/project/recipe/art
apps/feed        feed.farfield.systems — feed entries
apps/blobs       blobs.farfield.systems — shared blob service

cli              the `farfield` CLI
schemas/         lexicon-lite schema files (per app: content/, feed/)
deploy/          cloudflared config, systemd units, example farfield.toml
```

`content` and `feed` are thin binaries over `crates/records` — same engine,
different DB, schemas, and domain. RSS and rendering are the website's job.

## Status

Scaffolded. See the approved design doc for the full plan:
`~/.gstack/projects/mission-control/iammatthias-main-design-20260515-091028.md`

First implementation step (The Assignment): write the `schemas/` files from
real existing markdown frontmatter before filling in the Rust.

## Build

```sh
cargo build
cargo test
```
