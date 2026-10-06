#!/bin/sh
# A second device edits the same entry first (read-modify-write with If-Match,
# as any well-behaved client does). Used by conflict.json.
set -e
K='X-API-Key: dev-content-key'
B=http://127.0.0.1:8787
SLUG=$(curl -s -H "$K" "$B/api/entries?status=all&limit=100&bodies=0" | jq -r '[.entries[] | select(.title | test("photograph"))][0].slug')
U=$B/api/entries/$SLUG
T=$(mktemp)
ETAG=$(curl -s -D - -o "$T" -H "$K" "$U" | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')
jq '.body = "Edited on another device first.\n\n" + .body | .title = (.title + " (retitled elsewhere)")' "$T" \
 | curl -s -o /dev/null -w "other device PUT %{http_code}\n" -X PUT -H "$K" -H "If-Match: $ETAG" -H 'Content-Type: application/json' --data-binary @- "$U"
rm -f "$T"
