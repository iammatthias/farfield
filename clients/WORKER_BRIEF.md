# Farfield Desktop — workspace brief (for implementers)

Repo: /Users/iammatthias/Developer/farfield, branch `desktop-client`. Do NOT commit, push, or deploy. Other agents are editing other files in the same tree at the same time: touch ONLY the files your task names.

## Read first (the reference pattern)
- `clients/desktop/src/ws/content.rs` — the reference workspace: list (uniform_list, paging, Latest ticket to drop stale responses, ETag-revalidated loads via Session::load, Freshness → offline banner), keyboard nav from the filter field (FieldEvent::Up/Down/Submit), inspector, confirm() before destructive/publishing actions, toasts, set_health.
- `clients/desktop/src/ui/draft_doc.rs` — a document backed by a local draft (autosave to disk, ⌘S server save with If-Match, conflict resolution, blob uploads with progress/cancel). Reuse it for any long-text editing (feed posts use `farfield_core::sync::FeedPost`).
- `clients/desktop/src/ui/mod.rs` (button, rule, eyebrow, mono, chip, notice, list_row, field_row, floating, bytes, when), `ui/input.rs` (TextField: `.mono()`, `.secret()`, events), `ui/doc_editor.rs` (the editor.wasm host; `set_plain(true)` for code/plain text).
- `clients/desktop/src/shell.rs` (confirm, toast, goto, set_health), `app.rs` (session(cx), describe(err), log(event, fields)), `workspace.rs` (Workspace trait: inspector, palette, focus_search, new_item, save, refresh, dirty).
- `clients/core/src/api/*` — typed clients. `misc.rs` already has bookmarks/qr/scrap/library/sideload/daily/pulse/switchboard/backup/apex modules written against the contract below; fix them if they disagree with the server, and put NEW client functions in your group's `clients/core/src/api/ext_<group>.rs` (already created and declared).
- `docs/API.md` (incl. the new "Private admin API" section) and `docs/BRAND.md` §11–13 (product UI, buttons, forms).

## Rules
- All I/O off the UI thread: `farfield_core::spawn(async move { ... })` then `cx.spawn(async move |this, cx| { let r = task.await; this.update(cx, ...) })`.
- Never follow redirects; errors are typed (`ApiError`); show them with `app::describe`. Auth failures → `set_health(cx, svc, Health::NoAuth)`; offline → `Health::Down`.
- Cached reads (`Session::load`) return `Freshness::Stale` when offline — show the stale banner like content.rs does; the service being down must not break other workspaces.
- Explicit confirmation (shell::confirm) for every delete/revoke/publish-like action. Mutations that aren't idempotent are never retried automatically.
- Farfield v5: no boxes; structure from space and full-width rules; Inter chrome, Newsreader only for document text, Plex Mono for technical readouts (CIDs, sizes, dates, URLs). Accent fill only on the one primary action. Shadows only on floating things.
- Keyboard: ⌘F focuses your filter field; Up/Down from it move the selection; Enter opens/acts; ⌘N new where it makes sense; ⌘R refresh; ⌘S save where there's a form.
- Share/public links come from `session.public_base(svc)`; API calls only via the private endpoint (the client).
- Clipboard: `cx.write_to_clipboard(gpui::ClipboardItem::new_string(..))`. File dialogs: `cx.prompt_for_paths(gpui::PathPromptOptions{..})` / `cx.prompt_for_new_path(&dir, Some(name))`. Images: decode with the `image` crate off-thread, render via `gpui::img(Arc<RenderImage>)` (RenderImage wants BGRA — swap R/B; see doc_editor.rs) and `window.drop_image(old)` when replacing.
- Bound caches/concurrency: thumbnails cached by CID in a bounded LRU (e.g. 200 entries), at most ~6 concurrent fetches.
- `gpui` is 0.2.2 (crates.io). Its source is in ~/.cargo/registry/src/*/gpui-0.2.2 (examples/ are useful). Pixels' inner field is private: use `f32::from(px)`.

## Admin API facts (implemented server-side; all behind PrivateAPI: Cf-Ray/Cf-Connecting-IP → 404, no key configured → 503, non-write key → 401; Cache-Control no-store)
- bookmarks: GET /api/admin/bookmarks → {bookmarks:[Bookmark incl private, adminNotes]}; GET /api/admin/bookmarks/{id} (+ETag); POST /api/admin/bookmarks/{id}/refresh → Bookmark (502 if fetch fails). Writes: POST/PUT(partial)/DELETE /api/bookmarks[/{id}] with optional If-Match (CID) → 412 {error,current}.
- qr: GET /api/admin/codes → {codes:[Code]} (all); GET /api/admin/codes/{id}; GET /api/admin/codes/{id}/preview.svg; /preview.png?size=N (64..2048; actual px = scale*(modules+8)). Writes: POST/PUT(partial)/DELETE /api/codes[/{id}] (+If-Match). mode direct|proxy, ec L/M/Q/H, public, enabled. Proxy codes redirect via public /r/{id} (302).
- scrap: GET /api/admin/pastes?limit&page → {pastes:[Paste body ""],total}; GET /api/admin/pastes/{id} → with body; PUT /api/admin/pastes/{id} {title?,lang?,visibility?,expires?: never|1h|1d|1w|1m}. Create: POST /api/pastes raw body ?title&lang&visibility&expires&token=generate → text/plain "url\ntoken: x". DELETE /api/pastes/{id}; POST /api/pastes/{id}/token/roll → "token: x"; DELETE /api/pastes/{id}/token. Paste {id,cid,title,lang,body,visibility,expiresAt,createdAt,views,alias?,hasToken}. Token forces public→unlisted.
- library: GET /api/admin/books → {books:[Book],collections:[{name,count}],uncategorized:n}; POST /api/books raw EPUB ?filename&collection (small); tus at /api/upload/tus (see core/src/upload.rs tus_upload); PUT /api/books/{cid}/collection?collection= ; DELETE /api/books/{cid}; cover GET /opds/cover/{cid}.
- sideload: GET /api/builds; POST /api/builds raw IPA ?filename&notes; DELETE /api/builds/{id}; DELETE /api/apps/{bundle}; POST /api/builds/{id}/share?ttl=30m|2h|24h&max=1|3|unlimited&label → {token,shareURL,expiresAt,maxInstalls}; GET /api/admin/shares → {shares:[{token,buildId,appName,version,label,state:active|consumed|revoked,expiresAt,maxInstalls,installs,revoked,live,createdAt,consumedAt?,shareURL}]}; POST /api/admin/shares/{token}/revoke.
- feed: GET /api/posts?limit&before=createdAt|slug; POST /api/posts {body,tags}; POST /api/posts/media multipart; PUT /api/posts/{slug} {body,tags} (+If-Match = post CID); DELETE ?media=release (+If-Match).
- blobs: GET /blobs?page (48/page) {blobs,total,page,pages}; GET /blobs/{cid}; /blobs/{cid}/meta; POST /blobs raw; DELETE /blobs/{cid}?unlessReferenced=1 → 409 {error,references}. thumbCid for large images (fetch /blobs/{thumbCid}).
- daily (public): /api/photo, /api/photo/{date}, /api/photos?page (14/page), /api/art[/{date}], /art.svg, /art/{date}.
- pulse: /api/overview {targets:[{...Target,last,up24h,up7d,up30d,incident?}],incidents}, /api/traffic?app&from&to (PULSE_READ_KEY only; a bad key 303s → Unauthorized). Target administration = hand off to the console (connections::open_console(cx,"pulse")).
- switchboard: GET /api/admin/messages?limit → {messages:[{id,direction,sender,body,route,ref,reply,status,receivedAt}]}; GET /api/admin/jobs?limit → {jobs:[{id,messageId,sender,prompt,status,result,error,startedAt,finishedAt}]}. Read-only.
- backup: GET /api/admin/snapshots → {snapshots:[{app,cid,size,createdAt}]} (BACKUP_API_KEY only). Observational only — no actions.
- keys: no API by design. Workspace = explain + hand off to the private console (connections::open_console(cx,"keys")) + show which services have keys stored (hints only).
- apex: /status, /api/profile {sections:{feed?,writing?,daily?},updatedAt}; plus a fleet health overview from AppState.health.

## Testing
- Integration tests go in `clients/core/tests/<your-group>.rs` with `mod fleet;` (the harness in tests/fleet/mod.rs starts the REAL Go services from this checkout on ephemeral ports; see tests/integration.rs for usage: Fleet::start(&[..]), f.admin(), f.session_with(..), f.mint(..), f.revoke(..), f.stop/restart, block(..)). Cover: auth (no key/wrong key/read vs write), visibility (private items only via admin), uploads/progress/cancel where relevant, pagination, caching (ETag 304 → Freshness::Live from cache), conflicts (If-Match 412) where relevant, revocation, private-route isolation (a raw reqwest request with header `Cf-Ray: x` to an /api/admin route → 404), interruptions (f.stop mid-operation → Offline/Uncertain, then recovery).
- Run: `cd clients && cargo test -p farfield-core --test <your-group>` and `cargo build -p farfield-desktop`. Also `cargo clippy -p farfield-desktop -p farfield-core -- -D warnings` must stay clean for YOUR files.
- Look at your UI rendered: the dev fleet runs on standard ports (`make dev` already started; password demo, keys dev-<app>-key). Run the app with a script (see clients/desktop/src/evidence.rs header):
  ```
  SP=<your scratch dir>; cd clients && env FARFIELD_DESKTOP_DATA=$SP/data FARFIELD_PROFILE=local FARFIELD_SECRETS=memory FARFIELD_KEY_FEED=dev-feed-key ... FARFIELD_SCRIPT=$SP/s.json FARFIELD_EVIDENCE_DIR=$SP/out timeout 60 ./target/debug/farfield-desktop
  ```
  Script steps: {"wait":ms} {"key":"cmd-2"} {"type":"text"} {"drop":["/abs/file"]} {"snap":"name"} {"until":"event","timeout":ms} {"quit":true}. Workspaces by ⌘1..⌘9 in order: content, feed, blobs, bookmarks, library, daily, qr, scrap, sideload (then pulse, switchboard, backup, keys, apex via palette "Go to …"). Then Read the PNGs and fix what looks wrong. Log notable actions with app::log so scripts can `until` them.
- Final report: what you built, tests added and their results (exact commands + pass counts), screenshots paths you looked at, anything not done.

## Design ambition (from the user: "have fun building a beautiful UI that presents everything cleanly and functionally")
Don't stop at a list + form. Design each workspace for what its content IS, within v5:
- Media (blobs, feed photos, library covers, daily art) deserves image-forward layouts: a calm thumbnail grid with generous gutters, the image itself as the hero in the inspector, dominant-color placeholders (blobs Meta.dominantColor) while thumbnails load, aspect ratios respected.
- Readouts (sizes, CIDs, dates, uptime, latency) in Plex Mono, aligned in tidy columns; small sparkline/bars for pulse traffic and uptime (draw with div bars or gpui paths — no chart library), Horizon orange only for the one thing that needs attention.
- Empty, loading and offline states are designed moments (a sentence that says what to do next), not blank panes.
- Hierarchy through type size/weight and space; full-width horizon rules between sections; generous padding; nothing cramped. Hover states, selected states, focus states visible.
- Look at every screen you build (screenshot via the script) in light AND dark (palette "Theme: cycle…" or ⌘⌥T), and iterate until it's genuinely good.
