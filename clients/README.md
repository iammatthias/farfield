# Farfield — native client

A Rust/GPUI macOS app for the whole fleet: every service's workspace in one window,
working over the tailnet, with local drafts that survive outages and relaunches.

```
clients/
  editor/    the editor host: lib/editor's editor.wasm under wasmtime (Cranelift; Pulley on iOS)
  core/      portable, no UI: fleet registry, profiles, Keychain credentials, transport,
             typed service clients, ETag cache, drafts, conflict-safe sync, uploads (incl. tus)
  desktop/   the GPUI app (Farfield v5), packaging/ for the .app
```

Go keeps everything it owned — business logic, publishing, content addressing, jobs,
authorization. The client owns UI, drafts, caching and typed async clients. Nothing is
copied from Go by hand: the editor's bytes come from `lib/editor/cmd/export`, the service
list and quick actions from `lib/capability/cmd/manifest` (lib/fleet + lib/capability), the
theme tokens and fonts from `lib/theme/theme.css`.

## Run it

Prerequisites: Rust (stable, 1.85+), Go (the workspace's version), Xcode command-line tools.
No Metal toolchain download is needed (GPUI is built with `runtime_shaders`).

```sh
make dev                                   # the local fleet on 127.0.0.1 (password demo)
cd clients && cargo run -p farfield-desktop
```

The first launch opens **setup**: where the fleet is (Tailscale is detected and the homelab
offered; type any tailnet name, or `127.0.0.1` for the dev fleet), the keys to it (one key
for every service, or one per service — each is tested live), and how it should look.
Everything is in **Settings** (⌘,) afterwards: the fleet address, a per-service address
override, profiles, keys, appearance, local data (show in Finder, clear cache), and
**Run setup again**. Keys go to the Keychain, bound to that endpoint's origin, and are never
shown again (only a hint). For the dev fleet each app's key is `dev-<app>-key`.

### Against the homelab, over the tailnet

1. Tailscale running on this Mac (the OS app; the client never runs its own).
2. On the homelab, each service needs a private HTTPS address on the tailnet —
   `tailscale serve` per port, like backup already has:
   ```sh
   for p in 8787 8788 8789 8790 8792 8793 8794 8797 8798 8799 8800 8801 8802; do
     sudo tailscale serve --bg --https=$p http://172.17.0.1:$p
   done
   ```
   (switchboard runs on the host: use `http://127.0.0.1:8802` for it.) Consoles opened from
   the app (keys, pulse, backup) use these private addresses too; like backup, a console
   reached on a `ts.net` name needs `SESSION_COOKIE_DOMAIN` empty for its sign-in cookie to
   stick, or it loops back to the login page. API and media traffic
   then stays on the tailnet and keeps working when Cloudflare or the tunnel doesn't.
   The new `/api/admin/*` routes refuse anything that arrives through the tunnel
   (`Cf-Ray`/`Cf-Connecting-IP` → 404), so they are reachable only this way.
3. Deploy the Go changes on this branch (`ff-deploy farfield`) and set `BACKUP_API_KEY` in the
   homelab `.env` (backup's admin route fails closed without it).
4. Setup (or Settings → Fleet address) → **Detect with Tailscale** → **Use this address** → keys. Mint scoped
   `ffk_` keys in the keys console (opened from Connections; its password is typed in the
   browser, never in this app). Use a `write` key per app for full use; `read` keys see only
   public data.

## Using it

- ⌘1–⌘9 switch workspaces (Content, Feed, Blobs, Bookmarks, Library, Daily, QR, Scrap,
  Sideload; Pulse, Switchboard, Backup, Keys, Apex from the palette). ⌘K / ⇧⌘P palette,
  ⌘F filter the current list (↑/↓ move, ↩ opens), ⌘N new, ⌘S save to server, ⌘R refresh,
  ⌘\ navigation, ⌥⌘I inspector, ⌥⌘T theme (system/light/dark), ⌘, Settings.
  Navigation and inspector resize by dragging their edges.
- Documents: every edit is saved on this Mac (atomically) a moment after you stop typing —
  the inspector says "Saved on this Mac · not on the server" until you ⌘S. Server saves are
  conditional on the version you opened; if someone else saved first you get a conflict
  with both versions kept: **Merge for review**, **Keep mine**, or **Take theirs**. Uploads
  you added are never dropped by a resolution.
- Publishing, unpublishing and deleting always ask first (↩ confirms, esc cancels).
- Offline: lists show what was last loaded and say how old it is; saves fail as "offline"
  and stay local; a save whose answer was lost is checked against the server before
  anything is resent. Other services keep working.
- Drop files on a document (or Insert file…) to upload to blobs and insert `blob://` refs.

## Tests

```sh
cd clients
cargo fmt --all --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace              # unit + integration (starts the real Go services)
FARFIELD_EDITOR_PULLEY=1 cargo test -p farfield-editor   # the iOS interpreter path
cd .. && make vet && make test      # the Go workspace
go test ./lib/editor -run Parity    # Go side of the editor parity goldens
```

Integration tests build the Go apps from this checkout and run them on ephemeral loopback
ports with throwaway data (the same environment `scripts/devfleet.sh` gives `make dev`).

## Build the .app

```sh
clients/desktop/packaging/build-app.sh                   # → clients/target/Farfield.app
REV=HEAD VERIFY=1 clients/desktop/packaging/build-app.sh # from a clean worktree, twice, compared
```

Inputs are pinned (Cargo.lock `--locked`, pre-exported Go assets, `SOURCE_DATE_EPOCH`,
remapped paths); the bundle is ad-hoc signed. Re-sign with a Developer ID to distribute.
An ad-hoc signature changes with every build, so macOS may ask again for Keychain access
after you install a new build; a stable signing identity avoids that.

## Evidence runs

```sh
make dev
clients/desktop/evidence/run.sh            # every scenario → clients/target/evidence/
clients/desktop/evidence/run.sh conflict   # one
```

Scenarios: first-run setup, a light and a dark tour of every workspace, media publishing
(drop a HEIC → blob → publish), a conflict with a second device and its merge, an outage
mid-edit and recovery of the draft after a relaunch, inserting a blob into the open
document, and a half-written post surviving a quit. Each replays through the real input
path (`FARFIELD_SCRIPT`) and captures the app's own window (no Screen Recording
permission needed); each run's structured `events.jsonl` sits beside its screenshots.
Scripted runs keep keys in memory (`FARFIELD_SECRETS=memory`, `FARFIELD_KEY_<SERVICE>`).
A script's `shell` step runs arbitrary commands — scripts are development tooling and run
only when `FARFIELD_SCRIPT` is set.

## iOS

`farfield-core` and `farfield-editor` build for `aarch64-apple-ios`, and the editor passes
the parity suite under wasmtime's Pulley interpreter (iOS forbids JIT). GPUI itself has no
official iOS backend yet (zed-industries/zed#63068 is open), so the iOS UI is not built; the
shared core is ready for it, or for a SwiftUI shell over UniFFI.
