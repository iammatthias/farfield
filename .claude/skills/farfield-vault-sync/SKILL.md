---
name: Farfield Vault Sync
description: Bidirectional sync between the Obsidian content vault and the farfield content service via `content sync-vault`. Use when authoring flows touch the vault, when vault and production content drift, or when reconciling copy edits made in Obsidian against content.farfield.systems.
---

# Farfield Vault Sync

## Overview

The Obsidian vault at
`~/Library/Mobile Documents/iCloud~md~obsidian/Documents/obsidian_cms` is the
authoring surface for content.farfield.systems. Its `content/` directory maps
1:1 onto the service: each subfolder is a collection, each `.md` file an
entry, frontmatter is `title, slug, published, created, updated, tags,
excerpt`. Files directly in `content/` (e.g. VOICE_DNA.md) are notes, not
entries, and are ignored.

`content sync-vault <content-dir>` (apps/content/sync.go) syncs both ways.

## How it decides

Three-way merge against `content/.farfield-sync.json`, which records each
entry's remote CID + local hash at last sync:

- local changed → push (PUT; server stamps updatedAt, file is rewritten to match)
- remote changed → pull (file rewritten in vault frontmatter style)
- both changed → last write wins (default `--prefer newer`): the more
  recently edited side is taken — local edit time is max(file mtime,
  frontmatter `updated`), remote is the server's `updatedAt`; ties fall back
  to a `<slug>.remote.md` sibling. `--prefer manual|local|remote` overrides
  for bulk reconciliation. Losers are recoverable (remote → entry revision
  history, local → vault git)
- never-synced + equal content → state seeded quietly (no updatedAt churn)
- new local file → create (POST; the API honors the vault's `created` date)
- new remote entry → written into the vault
- deletions NEVER propagate — a vanished side is reported, nothing is removed

## The Obsidian plugin (primary UX)

`obsidian-plugin/farfield-sync/` is the in-app version — same merge, same
state file, hash byte-compatible with Go (a cross-tool test proved parity).
Installed at `<vault>/.obsidian/plugins/farfield-sync/`; settings (URL, write
key, conflict preference, auto-sync interval) live in its `data.json`, which
is gitignored in the vault because it holds the key. Ribbon icon / command
palette syncs on demand; auto-sync runs while Obsidian is open. The CLI
below remains for scripting and bulk reconciliation.

## Running the CLI

```sh
cd apps/content && go build -o /tmp/ff-content .
CONTENT_URL=https://content.farfield.systems \
BLOBS_PUBLIC_URL=https://blobs.farfield.systems \
CONTENT_API_KEY=<write key> \
/tmp/ff-content sync-vault --dry-run "<vault>/content"   # always dry-run first
```

The write key lives in the deployment `.env`, never in this repo. Read it into
the environment without printing it — the `farfield-deploy` skill (local, not
committed) has the host and path. The client sends a browser-shaped
User-Agent, because the Cloudflare edge rejects bot agents.

## Media rules (history that bites)

- Bodies reference media as `blob://<cid>`; galleries as `![](series://<slug>)`.
- Legacy `ipfs://<cid>` refs: raw (bafkrei…) CIDs are IDENTICAL to blob CIDs
  — `--migrate-refs` rewrites them after verifying each against the blob
  store. dag-pb (bafybei…) CIDs re-hashed during the 2026 migration and do
  NOT match; those were fixed once by positional mapping against production
  bodies. A push refuses bodies with CID-bearing ipfs refs; prose that merely
  mentions `ipfs://` is fine.
- Production galleries use curated `series://` embeds — never overwrite them
  with raw image lists from old vault copies.

## Conventions

- Slugs are stable public URLs: production's slug wins identity disputes;
  rename the vault file, don't re-slug production.
- New entries via Templater templates in `<vault>/templates/` (they scaffold
  the full frontmatter including excerpt).
- The vault is its own git repo (auto-backup commits) — make an explicit
  checkpoint commit before bulk reconciliation.
