#!/usr/bin/env bash
# Measure the desktop journeys against the local dev fleet.
#
#   make dev
#   clients/desktop/perf/run.sh [runs=5] [check]   → median / p95 per journey;
#                                               "check" compares with baseline.txt
#
# Each run is a cold launch (fresh process, warm OS file cache) that opens an
# entry, types, saves, and switches workspaces. Spans come from the app's own
# `perf` events (src/perf.rs): cold-first-frame, cold-content-list, doc-open,
# key-to-frame, save, switch:<workspace>.
set -euo pipefail
cd "$(dirname "$0")/../../.."
RUNS=${1:-5}
BIN=$PWD/clients/target/release/farfield-desktop
[ -x "$BIN" ] || (cd clients && cargo build -q --release -p farfield-desktop)
OUT=$PWD/clients/target/perf
rm -rf "$OUT"; mkdir -p "$OUT"
for i in $(seq 1 "$RUNS"); do
  D=$OUT/run$i; mkdir -p "$D"
  printf '{"mode":"light","onboarded":true,"profile":"local","workspace":"content"}' > "$D/prefs.json"
  env FARFIELD_TOPMOST=1 FARFIELD_DESKTOP_DATA="$D" FARFIELD_PROFILE=local FARFIELD_SECRETS=memory \
    FARFIELD_KEY_CONTENT=dev-content-key FARFIELD_KEY_FEED=dev-feed-key FARFIELD_KEY_BLOBS=dev-blobs-key \
    FARFIELD_SCRIPT="$PWD/clients/desktop/perf/journey.json" FARFIELD_EVIDENCE_DIR="$D" timeout 120 "$BIN" >/dev/null 2>&1 || true
done
cat "$OUT"/run*/events.jsonl | jq -r 'select(.event=="perf") | "\(.span) \(.ms)"' > "$OUT/spans.txt"
printf "%-20s %6s %9s %9s\n" journey n median p95
for span in $(cut -d' ' -f1 "$OUT/spans.txt" | sort -u); do
  grep "^$span " "$OUT/spans.txt" | cut -d' ' -f2 | sort -g | awk -v s="$span" '
    {v[NR]=$1} END { m=v[int((NR+1)/2)]; p=v[int(NR*0.95+0.999)]; if(p=="")p=v[NR]; printf "%-20s %6d %9.1f %9.1f\n", s, NR, m, p }'
done | tee "$OUT/summary.txt"

if [ "${2:-}" = check ] || [ "${CHECK:-}" = 1 ]; then
  fail=0
  while read -r span base; do
    case "$span" in ''|\#*) continue ;; esac
    got=$(awk -v s="$span" '$1==s {print $3}' "$OUT/summary.txt")
    [ -n "$got" ] || { echo "missing: $span"; fail=1; continue; }
    if awk -v g="$got" -v b="$base" 'BEGIN{exit !(g > b*1.5)}'; then
      echo "SLOWER: $span median $got ms > 1.5 × $base"; fail=1
    fi
  done < "$PWD/clients/desktop/perf/baseline.txt"
  [ $fail = 0 ] && echo "all journeys within 1.5× of baseline"
  exit $fail
fi
