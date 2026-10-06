# Performance

The method, borrowed from how claude.ai got 3× faster: name the journeys a
person feels, measure them reproducibly, fix the largest cost, then lock the
gain in with a check that fails if it regresses. Measure first — every change
below was preceded by a number that said where the time went.

## Journeys and how to measure them

| Surface | Journey | Measure with |
|---|---|---|
| Desktop | cold start → first frame → first list | `clients/desktop/perf/run.sh 5 check` |
| Desktop | open an entry → editor painted (`doc-open`, with laps) | same |
| Desktop | keystroke → frame (`key-to-frame`, split into host prep and image upload) | same |
| Desktop | save to server; switch workspace → list | same |
| Editor | instance, dictionary, set text, insert+render, framebuffer copy | `cargo test --release -p farfield-editor --test perf -- --ignored --nocapture` |
| APIs | latency, bytes raw/gzip, ETag and 304 per read | `go run ./lib/capability/cmd/apiperf` |
| APIs | every list the client reads revalidates (guard) | `cargo test -p farfield-core --test integration every_list` |

The desktop spans come from the app itself (`clients/desktop/src/perf.rs`,
logged as `perf` events in `events.jsonl`); `run.sh` launches the release build
cold several times against the dev fleet and reports median and p95. Baselines
live in `clients/desktop/perf/baseline.txt` and
`clients/editor/tests/perf-baseline.json`; the checks fail at 1.5× a baseline.
Lower a baseline when something gets faster; never raise one to pass.

**Frame-timed journeys need an unlocked screen.** macOS stops drawing a
window that is covered, and draws nothing while the session is locked — then
`doc-open` and `key-to-frame` measure the wait for a frame that isn't coming
(~850 ms and ~20 ms in a locked run). `run.sh` keeps the window topmost
(`FARFIELD_TOPMOST=1`) so a covered window isn't the problem; a locked session
still is, which is why those two journeys are not in the committed baseline.
Their CPU parts (`doc-fetch`, `doc-build`, `doc-open@from-cache`,
`frame-host-prep`, `frame-paint-image`) are.

## Results (2026-10-06, Apple Silicon, dev fleet on loopback)

### Desktop (release, median of 5 cold launches)

| Journey | Before | After | Change |
|---|---|---|---|
| Cold start → first frame | 157 ms | 137 ms | editor compile moved off the main thread |
| Cold start → content list | 164 ms | 144 ms | |
| Fetch an entry | 17 ms | 6.8 ms | response cache no longer fsyncs |
| Build the open document, p95 | 58 ms | 13 ms | editor module prewarmed at launch |
| Open an entry already hovered/neighbouring | (network) | 11.8 ms, no network | open from cache, revalidate after |
| Switch → Feed / Blobs list | 25 / 28 ms | 11 / 15 ms | |
| Keystroke: host CPU per frame | 5.1 ms | 4.3 ms | inside a 120 Hz budget (8.3 ms) |

### Editor host (`perf.rs`, median ms)

first instance 34.6 (compile) · new instance 0.30 · dictionary 3.5 ·
set text + render (10k words) 4.4 · insert + render 4.7 · framebuffer copy 1.2

### APIs (`apiperf`, p50)

Server time was already under 1 ms everywhere; the cost a client feels is round
trips and bytes. So:

| Read | Before | After |
|---|---|---|
| 11 list reads (blobs, admin lists, builds, daily archive) | no ETag: full body every refresh | ETag + 304 |
| Bodies under 1 KiB | gzip made them bigger (24 B → 49 B) | sent plain |
| Editor dictionary (1 MB) | 14.6 ms (gzipped per request), no ETag | 0.27 ms (precompressed once), ETag + 304, 275 KB |
| Console blob picker (proxied list) | re-downloaded every open | revalidates through the proxy |

## What changed, and why it is safe

- **Revalidation everywhere** (`web.WriteJSONValidated`): the ETag is the CID
  of the response bytes, so it is correct by construction — no version
  bookkeeping to forget. Cache-Control is left to each route (admin routes keep
  `no-store`; a 304 still works for a client that keeps its own copy).
- **gzip threshold** (`lib/web` Gzip): responses are held to 1 KiB before
  deciding; a handler that flushes early is streaming and still gets gzip.
- **Precompressed editor assets** (`lib/editor/assets.go`): compressed once per
  process at best compression, `Vary: Accept-Encoding`, ETag per encoding.
- **Dictionary fetched when idle** (`lib/editor/host.js`): first load of an
  editor page no longer races fonts and wasm for 1 MB of word list; spelling
  marks appear a moment later.
- **Desktop: open from cache, then revalidate.** A hovered row (or the rows
  either side of a keyboard selection) is prefetched; opening it uses the cached
  copy and checks the server right after. A newer server copy replaces the
  document only while it is untouched; once edited, the conditional save
  decides (a conflict, never an overwrite) — covered by
  `cached_open_is_instant_and_revalidates`.
- **Desktop: the cache is disposable, drafts are not.** Response-cache writes
  are atomic (temp + rename) but not fsynced, and disk eviction runs only when a
  running size estimate passes the cap. Drafts and preferences still fsync.
- **Desktop: editor prewarm and text cache.** `editor.wasm` is compiled on a
  background thread at launch; the document text is cached per revision for the
  platform input system's repeated queries.

## Not done (measured, judged not worth it yet)

- Uploading only the dirty band of the editor framebuffer: the full-frame
  upload is ~1.2 ms of a 4.3 ms keystroke, well inside a 120 Hz frame.
- The editor's own render (~3 ms per keystroke in wasm) — the larger share, and
  a change to `lib/editor/wat`, which every host shares; worth a dedicated pass.
- wasm and font assets are not precompressed (binary); they carry ETags and are
  immutable when versioned.
