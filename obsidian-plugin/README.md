# Farfield Publisher

An Obsidian plugin that publishes notes and media to a Farfield backend over
its authenticated HTTP API. Every request goes through Obsidian's `requestUrl`,
so it works on **desktop and mobile** — no CLI, no binary to install.

Content and the feed are distinct:

- **Content pieces** belong to collections (`posts`, `art`, `recipes`, …) and
  live in the content folder, one subfolder per collection.
- **Feed posts** are a single stream of short posts in a `feed/` folder.

## Build

```sh
cd obsidian-plugin
npm install
npm run build      # type-checks, then bundles main.ts -> main.js
```

`npm run dev` rebuilds on change.

## Install

Copy `manifest.json` and the built `main.js` into your vault:

```
<vault>/.obsidian/plugins/farfield-publisher/
```

Then enable **Farfield Publisher** in Obsidian → Settings → Community plugins.

## Configure

| Setting | What |
|---------|------|
| Content service URL | e.g. `https://content.farfield.systems` |
| Feed service URL | e.g. `https://feed.farfield.systems` |
| Blob service URL | e.g. `https://blobs.farfield.systems` |
| Content folder | Vault folder holding the collection subfolders (default `content`) |
| Write token | `FARFIELD_TOKEN` — the bearer token for authenticated writes |

## Use

- **New content piece** — pick a collection and enter a title. The plugin
  reads that collection's live schema and creates a note pre-filled with a
  correct frontmatter skeleton (every declared field valid for its type,
  `created`/`updated` stamped, `slug` set), at
  `<content folder>/<collection>/<slug>.md`.
- **New feed post** — creates a short-post note straight in `feed/` — no
  collection, no title prompt. The feed is one stream.
- **Publish current note** — `PUT`s the note to its service. The note's
  **parent folder is the collection**; a note under `feed/` publishes to the
  feed service, every other folder to the content service. Frontmatter is
  projected onto the live schema — declared fields only, type-coerced; a
  missing required timestamp is stamped now. The record key is the `slug`
  field, or the filename.
- **Paste / drop media** — uploads to the blob store; the embed becomes
  `![](blob://<cid>)` (or a `[name](blob://<cid>)` link for non-visual files).
- **Check service status** — pings the content, feed, and blob services.
