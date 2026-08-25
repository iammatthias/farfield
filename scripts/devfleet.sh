#!/usr/bin/env bash
# devfleet — run the whole farfield fleet locally against throwaway data.
#
#   scripts/devfleet.sh start|stop|restart
#
# Binaries come from ./bin (make build). Data lives in ./tmp/dev — never the
# production /data layout. Every admin app uses password "demo" and the API
# keys below, so the apps can talk to each other locally.
set -euo pipefail
cd "$(dirname "$0")/.."

DATA=tmp/dev
LOGS=$DATA/logs
mkdir -p "$DATA/blobs-data" "$LOGS"

# app:port, in dependency order (blobs first — others upload to it)
FLEET="blobs:8789 content:8787 feed:8788 apex:8790 backup:8791 daily:8792 bookmarks:8793 qr:8794 library:8797 pulse:8798 scrap:8799 sideload:8800 keys:8801 switchboard:8802"

stop() {
  for pair in $FLEET; do
    port=${pair#*:}
    for pid in $(lsof -ti tcp:"$port" -sTCP:LISTEN 2>/dev/null); do
      kill "$pid" 2>/dev/null || true
    done
  done
  sleep 0.5
  echo "fleet stopped"
}

start() {
  for pair in $FLEET; do
    app=${pair%%:*}; port=${pair#*:}
    [ -x "bin/$app" ] || { echo "missing bin/$app — run make build"; exit 1; }
    envname=$(echo "$app" | tr 'a-z-' 'A-Z_')
    env HOST=127.0.0.1 PASSWORD=demo COOKIE_SECURE=false \
      FARFIELD_FLEET=local SESSION_SECRET=dev-fleet-secret \
      FARFIELD_DEV_TEMPLATES="$PWD/apps/$app" \
      FARFIELD_DEV_THEME="$PWD/lib/theme" \
      "${envname}_PORT=$port" \
      "${envname}_DB_PATH=$DATA/$app.sqlite" \
      "${envname}_API_KEY=dev-$app-key" \
      BLOBS_BACKEND=local BLOBS_DIR="$DATA/blobs-data" \
      SIDELOAD_DIR="$DATA/sideload-blobs" LIBRARY_TUS_DIR="$DATA/tus-staging" \
      BLOBS_SPOOL_DIR="$DATA/blob-spool" \
      BLOBS_URL=http://127.0.0.1:8789 BLOBS_API_KEY=dev-blobs-key \
      BLOBS_PUBLIC_URL=http://127.0.0.1:8789 \
      CONTENT_URL=http://127.0.0.1:8787 CONTENT_API_KEY=dev-content-key \
      CONTENT_PUBLIC_URL=http://127.0.0.1:8787 \
      "bin/$app" serve > "$LOGS/$app.log" 2>&1 &
  done
  sleep 1.5
  ok=0; down=""
  for pair in $FLEET; do
    port=${pair#*:}
    if curl -sf -m 2 "http://127.0.0.1:$port/status" | grep -q '"ok":true'; then
      ok=$((ok+1))
    else
      down="$down ${pair%%:*}"
    fi
  done
  echo "$ok/15 services up (password: demo)"
  [ -z "$down" ] || echo "DOWN:$down — see $LOGS/"
  echo "content admin: http://127.0.0.1:8787"
}

case "${1:-start}" in
  start) start ;;
  stop) stop ;;
  restart) stop; start ;;
  *) echo "usage: $0 start|stop|restart"; exit 2 ;;
esac
