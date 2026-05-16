#!/usr/bin/env sh
# Update a running farfield deployment: pull the latest commit and rebuild the
# Docker stack in place. The named volumes (the SQLite stores) survive the
# rebuild, so content is preserved.
#
# Run from anywhere — paths resolve relative to this script:
#
#   ./deploy/update.sh
set -eu

repo="$(cd "$(dirname "$0")/.." && pwd)"

echo "==> git pull --ff-only"
git -C "$repo" pull --ff-only

echo "==> docker compose up -d --build"
cd "$repo/deploy"
docker compose up -d --build

echo "==> running:"
docker compose ps --format 'table {{.Name}}\t{{.Status}}'
