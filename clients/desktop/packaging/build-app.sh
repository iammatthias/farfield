#!/usr/bin/env bash
# Build Farfield.app reproducibly.
#
#   clients/desktop/packaging/build-app.sh            → clients/target/Farfield.app
#   VERIFY=1 clients/desktop/packaging/build-app.sh   → build twice, compare
#
# Inputs are pinned: Cargo.lock (--locked), the Go workspace's editor and fleet
# exports (generated once into clients/target/inputs and fed to cargo, so the
# Rust build does not shell out to Go), SOURCE_DATE_EPOCH from the last commit,
# and path prefixes remapped so the checkout location does not leak into the
# binary. The bundle is ad-hoc signed (no identity needed); distribute with a
# Developer ID by re-signing it.
set -euo pipefail
cd "$(dirname "$0")/../../.."
REPO=$PWD
OUT=${OUT:-$REPO/clients/target/Farfield.app}
# REV=<commit> builds that commit from a clean worktree, so the result
# depends only on the commit (not on uncommitted edits in this checkout).
if [ -n "${REV:-}" ]; then
  WT=$REPO/clients/target/worktree
  git worktree remove --force "$WT" 2>/dev/null || true
  git worktree add --detach "$WT" "$REV" >/dev/null
  ROOT=$WT
  export CARGO_TARGET_DIR=$REPO/clients/target/worktree-target
else
  ROOT=$PWD
fi
cd "$ROOT"
CLIENTS=$ROOT/clients
INPUTS=$REPO/clients/target/inputs

export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct HEAD)}
export ZERO_AR_DATE=1
export RUSTFLAGS="--remap-path-prefix=$ROOT=/farfield --remap-path-prefix=$HOME/.cargo=/cargo --remap-path-prefix=$HOME/.rustup=/rustup"

build() {
  rm -rf "$INPUTS"
  mkdir -p "$INPUTS"
  go run ./lib/editor/cmd/export -out "$INPUTS/editor"
  go run ./lib/capability/cmd/manifest > "$INPUTS/fleet.json"
  export FARFIELD_EDITOR_ASSETS=$INPUTS/editor FARFIELD_MANIFEST=$INPUTS/fleet.json

  (cd "$CLIENTS" && cargo build --release --locked -p farfield-desktop --bin farfield-desktop --bin mkicon)

  VERSION=$(cd "$CLIENTS" && cargo pkgid -p farfield-desktop | sed 's/.*[#@]//')
  rm -rf "$OUT"
  mkdir -p "$OUT/Contents/MacOS" "$OUT/Contents/Resources"
  TGT=${CARGO_TARGET_DIR:-$CLIENTS/target}
  cp "$TGT/release/farfield-desktop" "$OUT/Contents/MacOS/Farfield"
  ICONSET=$TGT/AppIcon.iconset
  rm -rf "$ICONSET"
  "$TGT/release/mkicon" "$ICONSET"
  iconutil -c icns "$ICONSET" -o "$OUT/Contents/Resources/AppIcon.icns"
  sed "s/@VERSION@/$VERSION/g" "$CLIENTS/desktop/packaging/Info.plist" > "$OUT/Contents/Info.plist"
  # fixed timestamps, then sign (ad hoc: no identity, no secure timestamp)
  find "$OUT" -exec touch -h -t "$(date -r "$SOURCE_DATE_EPOCH" +%Y%m%d%H%M.%S)" {} +
  codesign --force --sign - --timestamp=none --options runtime "$OUT"
  codesign --verify --strict "$OUT"
  echo "built $OUT ($VERSION)"
}

digest() { (cd "$1" && find . -type f ! -path './Contents/_CodeSignature/*' -print0 | sort -z | xargs -0 shasum -a 256 | shasum -a 256 | cut -d' ' -f1); }

build
if [ "${VERIFY:-0}" = 1 ]; then
  FIRST=$(digest "$OUT")
  (cd "$CLIENTS" && cargo clean -p farfield-desktop --release)
  build
  SECOND=$(digest "$OUT")
  echo "bundle digest: $FIRST"
  if [ "$FIRST" = "$SECOND" ]; then echo "reproducible: identical on rebuild"; else echo "NOT reproducible: $SECOND"; exit 1; fi
fi
if [ -n "${REV:-}" ]; then
  cd "$REPO" && git worktree remove --force "$WT" && git worktree prune
fi
