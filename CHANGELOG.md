# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## [Unreleased]

### Added
- Vault sync conflicts resolve by last edit (file mtime / frontmatter `updated` vs the server's `updatedAt`); ties keep the `.remote.md` sibling fallback. The CLI's `--prefer` defaults to `newer` with manual/local/remote as overrides.
- Farfield Sync Obsidian plugin (`obsidian-plugin/farfield-sync`): the vault syncs from inside Obsidian — ribbon/command/auto-interval, settings-held credentials, same three-way merge and state file as the CLI with byte-compatible hashing (mobile-capable, no build step).
- `content sync-vault`: bidirectional Obsidian vault sync — three-way merge with a state file, push/pull/conflict handling, verified ipfs→blob ref migration, and quiet state seeding for already-identical entries. The create API honors a provided `createdAt` so vault-authored dates survive.
- Blob hygiene: blobs' `/hygiene` scans every entry, series, and post for blob:// references and reports referenced blobs (with their references), orphans (deletable in place), and dangling refs; generated thumbnails are excluded as derived, and delete affordances hide if any source scan fails.
- Login pages carry each app's tile mark; the editor gains a keyboard cheat sheet (⌘/ or the ? toolbar button).
- Series galleries reorder by dragging thumbnails on the edit page — the markdown rewrites itself (alt text preserved) and autosaves.
- Fleet search: content's `/search` (linked from the fleet menu) ranks entries, series, feed posts, and public bookmarks with on-device semantic embeddings — the corpus aggregates server-side over the read APIs, ranking never leaves the browser.
- Fleet single sign-on: set a shared `SESSION_SECRET` (and `SESSION_COOKIE_DOMAIN` for subdomain deployments) and one login works across every admin app via signed, stateless session cookies; logout anywhere ends the fleet session. Apps without the secret keep their own database sessions.
- `lib/markdown`: one shared markdown pipeline (goldmark + farfield embeds) for every admin UI — blob:// media, `[file](blob://cid)` links, series:// galleries, image alt text, and an editable-HTML renderer whose unsupported blocks round-trip verbatim.
- Rich document editor in the shared theme: contenteditable surface with formatting toolbar, markdown typing shortcuts, ⌘B/⌘I/⌘K, paste/drop upload, alt-text editing, autosave, async ⌘S saves, and a Markdown source toggle. Content and feed composers are document-first with a sticky metadata rail.
- Content: revision history with restore, trash with 30-day purge (no destructive admin actions remain), semantic entries search (vendored @ternlight/base, on-device WASM embeddings, CID-keyed caches), `SITE_URL_TEMPLATE` view-on-site links.
- Fleet switcher menu and per-app favicons across every admin masthead.
- Dev tooling: `make build/test/dev/e2e`, `scripts/devfleet.sh`, live template/theme reload via `FARFIELD_DEV_TEMPLATES`/`FARFIELD_DEV_THEME`, and a Playwright e2e suite asserting the editor's byte-identical round trip.

### Fixed
- `[text](blob://cid)` links used to render mangled markup; they now resolve to proper file links everywhere.
- iOS form-zoom and mobile overflow issues on the edit pages; the editor toolbar docks to the bottom edge on phones.

### Changed
- Theme v2 "warm instrument": bright warm ground, white panels, rounded corners, soft shadows, sentence-case labels, four-token palette with color-mix-derived tints, Display-P3 upgrades, and dark mode. All fifteen apps re-skinned; apex docs and style docs updated to match.
- Feed renders through `lib/markdown` (its local renderer is gone) with hard-wrap semantics preserved.
- CI: gofmt gate + workspace tests + fleet e2e on master pushes and PRs.
- Rename the calendar app to daily; the photo pages move under `/photo` and the old `/`, `/day/{date}`, and `/archive` paths redirect there. The JSON API paths are unchanged.
- Fix Farfield docs nav and logged-in screenshots (#14)
- Expand Farfield apex docs (#10)
- Polish bookmark form control spacing (#7)
- Build bookmarks app (#1)
