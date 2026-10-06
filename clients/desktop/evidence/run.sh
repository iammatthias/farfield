#!/usr/bin/env bash
# Replay the evidence scenarios against the local dev fleet and capture the
# app's own window (no Screen Recording permission needed).
#
#   make dev                                   # the fleet, on 127.0.0.1
#   clients/desktop/evidence/run.sh            # → clients/target/evidence/
#   clients/desktop/evidence/run.sh media      # one scenario
#
# Scenarios: onboard tour-light tour-dark media conflict outage+recover
# insert composer. Each gets its own data directory, keys come from the
# environment (FARFIELD_SECRETS=memory — the real Keychain is never touched),
# and every run's structured events land beside its screenshots. The outage
# scenario stops the dev content service and restarts the fleet afterwards.
set -euo pipefail
cd "$(dirname "$0")/../../.."
REPO=$PWD
EV=$REPO/clients/desktop/evidence
OUT=$REPO/clients/target/evidence
BIN=$REPO/clients/target/debug/farfield-desktop
mkdir -p "$OUT/assets"
(cd clients && cargo build -q -p farfield-desktop)
[ -f "$OUT/assets/horizon.heic" ] || sips -Z 1800 "/System/Library/Desktop Pictures/iMac Orange.heic" --out "$OUT/assets/horizon.heic" >/dev/null

KEYS=()
for a in content feed blobs bookmarks qr scrap library sideload switchboard backup; do
  KEYS+=("FARFIELD_KEY_$(echo "$a" | tr a-z A-Z)=dev-$a-key")
done

# run <name> <script> [mode] [keep-data]
run() {
  local name=$1 script=$2 mode=${3:-light} keep=${4:-}
  local data=$OUT/$name/data
  mkdir -p "$OUT/$name"
  if [ -z "$keep" ]; then
    rm -rf "$data"; mkdir -p "$data"
    [ "$name" = onboard ] || printf '{"mode":"%s","onboarded":true,"profile":"local","workspace":"content"}' "$mode" > "$data/prefs.json"
  fi
  sed -e "s#@EV@#$EV#g" -e "s#@ASSETS@#$OUT/assets#g" "$script" > "$OUT/$name/script.json"
  env FARFIELD_DESKTOP_DATA="$data" FARFIELD_PROFILE=local FARFIELD_SECRETS=memory "${KEYS[@]}" \
    FARFIELD_SCRIPT="$OUT/$name/script.json" FARFIELD_EVIDENCE_DIR="$OUT/$name" timeout 300 "$BIN" || true
  cp "$data/events.jsonl" "$OUT/$name/events.jsonl" 2>/dev/null || true
  echo "$name: $(ls "$OUT/$name"/*.png 2>/dev/null | wc -l | tr -d ' ') screenshots"
}

tour() {
  local mode=$1 i=1 s='[{"wait":2500}'
  for ws in content feed blobs bookmarks library daily qr scrap sideload; do
    s+=",{\"key\":\"cmd-$i\"},{\"wait\":3500},{\"key\":\"cmd-f\"},{\"key\":\"down\"},{\"wait\":2500},{\"snap\":\"$(printf %02d $i)-$ws\"}"
    i=$((i+1))
  done
  for ws in Pulse Switchboard Backup Keys Apex Settings; do
    s+=",{\"key\":\"cmd-shift-p\"},{\"type\":\"go to $ws\"},{\"key\":\"enter\"},{\"wait\":3500},{\"key\":\"cmd-f\"},{\"key\":\"down\"},{\"wait\":2000},{\"snap\":\"$(printf %02d $i)-$(echo $ws | tr A-Z a-z)\"}"
    i=$((i+1))
  done
  echo "$s,{\"quit\":true}]" > "$OUT/tour-$mode.json"
  run "tour-$mode" "$OUT/tour-$mode.json" "$mode"
}

want=${1:-all}
has() { [ "$want" = all ] || [ "$want" = "$1" ]; }
has onboard && run onboard "$EV/onboard.json"
has tour-light && tour light
has tour-dark && tour dark
has media && run media "$EV/media.json"
has conflict && run conflict "$EV/conflict.json"
if has outage; then
  run outage "$EV/outage.json"
  scripts/devfleet.sh restart | tail -1
  # the same data directory: the relaunch must find the unsaved draft
  cp -R "$OUT/outage/data" "$OUT/recover-data" 2>/dev/null || true
  rm -rf "$OUT/recover"; mkdir -p "$OUT/recover"; mv "$OUT/recover-data" "$OUT/recover/data"
  run recover "$EV/recover.json" light keep
fi
has insert && run insert "$EV/insert.json"
if has composer; then
  run composer "$EV/composer.json"
  run composer "$EV/composer-restore.json" light keep
fi
echo "evidence: $OUT"
