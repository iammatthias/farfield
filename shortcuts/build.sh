#!/usr/bin/env bash
# Build every farfield shortcut, signed via Apple's `shortcuts sign`.
#
#   ./build.sh             # default: --share=contacts (private to your contacts)
#   ./build.sh anyone      # --share=anyone (installable by any iCloud user)

set -eu

cd "$(dirname "$0")"

if ! command -v cherri >/dev/null; then
    echo "cherri not found — brew install electrikmilk/cherri/cherri" >&2
    exit 1
fi

share="${1:-contacts}"
case "$share" in
    contacts|anyone) ;;
    *) echo "share mode must be 'contacts' or 'anyone', got '$share'" >&2; exit 2 ;;
esac

for f in bookmarks feed qr; do
    printf '\n· %s.cherri  (share=%s)\n' "$f" "$share"
    cherri "$f.cherri" --share="$share" --no-ansi
done

printf '\n✓ built:\n'
ls -lh ./*.shortcut
