# Farfield

A self-owned content backend. One Rust service: content-addressed records,
lexicon-lite schemas, image blobs in R2, SQLite. It is the backend for
`iammatthias.com` — not the website and not the landing page, which are
separate Astro projects.

History is git's job (the authored markdown is versioned there). Farfield
holds current published state and serves it fast, with each record's CID as
its HTTP ETag.

## Layout

```
crates/core      record types, canonical encoding, CID
crates/store     SQLite schema + queries (rusqlite)
crates/schema    lexicon-lite loader + validator + TS codegen
crates/blobs     blob store: R2 + local-dir backends, GC
crates/snapshot  VACUUM INTO + R2 upload/restore + retention
server           the one binary — Axum records API
cli              the `farfield` CLI
schemas/         lexicon-lite schema files
deploy/          cloudflared config, systemd units, example farfield.toml
```

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
