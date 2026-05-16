# Farfield Publisher

An Obsidian plugin that publishes notes and media to a Farfield backend over
its authenticated HTTP API. Every request goes through Obsidian's `requestUrl`,
so it works on **desktop and mobile** — no CLI, no binary to install.

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
| Content service URL | e.g. `https://content.farfield.systems` |
| Feed service URL | e.g. `https://feed.farfield.systems` |
| Blob service URL | e.g. `https://blobs.farfield.systems` |
| Write token | `FARFIELD_TOKEN` — the bearer token for authenticated writes |

## Use

- **New note** — command palette → "Farfield Publisher: New note". Pick a
  collection and enter a title; the plugin reads that collection's live schema
  and creates a note pre-filled with a correct frontmatter skeleton (every
  declared field, valid for its type, `created`/`updated` stamped, `slug` set),
  in the right folder. Then write and publish.
- **Publish current note** — command palette → "Farfield Publisher: Publish
  current note". The note's **parent folder is the collection**. A note under a
  `feed` folder publishes to the feed service; every other folder publishes to
  the content service.
- **Paste / drop media** — paste an image or drop a file into a note; it
  uploads to the blob store and the embed becomes `![](blob://<cid>)` (or a
  `[name](blob://<cid>)` link for non-visual files).
- **Check service status** — pings the content, feed, and blob services.

The note's frontmatter is projected onto the collection's schema — only
declared fields are sent, others are dropped. A required timestamp that is
missing (common for quick feed posts) is stamped with the current time. The
record key is the `slug` frontmatter field, or the filename if there is none.
