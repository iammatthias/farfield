# Farfield Sync — Obsidian plugin

Bidirectional sync between a vault's `content/` directory and the farfield
content service, from inside Obsidian.

Two scopes, always in agreement (they share the merge logic and state file):

- **Vault-wide** — the circular-arrows ribbon icon, the "Sync vault with
  Farfield" command, or the optional auto-sync interval.
- **Single note** — the file-check ribbon icon (syncs the active note), the
  "Sync this note with Farfield" command, or right-click a note in the file
  explorer → "Sync with Farfield".

A status-bar summary reports what happened either way.

## Media upload (manual, per note)

"Upload this note's media to Farfield" (the image-up ribbon icon, command
palette, or the note's context menu) uploads the note's vault-local media —
`![[image.png]]`
embeds and `![alt](relative/path.png)` images — to the blobs service and
rewrites the references to `![alt](blob://cid)`. It needs a blobs write key
in settings (a scoped key from the keys app works).

It is deliberately **not** part of sync or auto-sync, and it never pushes
the entry: an uploaded blob only stops being an orphan once its entry
pushes, so media goes up when the author says so, and the entry follows on
the next sync of that note. Uploads are content-addressed — re-running is a
no-op per file. `file:///…` references point outside the vault and are
reported rather than uploaded.

It is the same three-way merge as `content sync-vault` (apps/content/sync.go)
and shares its state file (`content/.farfield-sync.json`) — the CLI and the
plugin are interchangeable. The content hash is byte-compatible with the Go
implementation (verified by a cross-tool test). Conflicts resolve to the
most recently edited side (local edit time = the later of file mtime and the
frontmatter `updated` stamp; remote = the server's `updatedAt`); ties write
a `.remote.md` sibling. Losing versions stay recoverable — remote in the
entry's revision history, local in the vault's git. Deletions never
propagate; bodies with legacy CID-bearing `ipfs://` refs refuse to push.

Plain CommonJS, no build step, no Node APIs — desktop and mobile.

## Install

Copy `manifest.json` + `main.js` into
`<vault>/.obsidian/plugins/farfield-sync/`, enable it, and set the content
URL + write API key in settings. Keep the plugin's `data.json` out of any
vault git remote — it holds the key.
