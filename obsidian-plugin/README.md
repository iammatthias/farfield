# Farfield Publisher

An Obsidian plugin — a thin wrapper over the `farfield` CLI. It publishes the
current note to a Farfield content backend and uploads pasted or dropped media
(images, video, audio, PDFs) to its blob store.

The CLI does the real work — frontmatter parsing, schema validation, the
blob/media records. This plugin is just the Obsidian-side ergonomics, so it is
**desktop-only** (spawning the binary needs Node).

## Prerequisites

- The `farfield` CLI binary, built from this repo: `go build -o farfield ./apps/farfield`
- A reachable Farfield deployment (content + blob services)

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

In the plugin's settings tab:

| Setting | What |
|---------|------|
| Farfield binary | Absolute path to the compiled `farfield` binary |
| Content service URL | e.g. `https://content.farfield.systems` |
| Blob service URL | e.g. `https://blobs.farfield.systems` |
| Schema directory | Path to this repo's `schemas/content` |
| Write token | `FARFIELD_TOKEN` — passed to the CLI for authenticated writes |

## Use

- **Publish current note** — command palette → "Farfield Publisher: Publish
  current note". The note's collection is its parent folder (`farfield push`).
- **Paste / drop media** — paste an image or drop a file into a note; it
  uploads to the blob store and the embed becomes `![](blob://<cid>)` (or a
  `[name](blob://<cid>)` link for non-visual files).
- **Check service status** — pings the content service.
