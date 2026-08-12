---
name: Farfield Sideload
description: Distribute iOS builds through sideload.farfield.systems — archive and export a signed IPA with xcodebuild, upload it via the API, hand out one-tap OTA install links and expiring share links, and manage device UDID registration. Use when asked to sideload an iOS app, ship a build to an iPhone without TestFlight, share an .ipa, or register a test device.
---

# Farfield Sideload — iOS build distribution

## Overview

`https://sideload.farfield.systems` is the self-hosted ad-hoc `.ipa`
installer (app `sideload` in the farfield monorepo, port 8800). It takes a
signed iOS build, content-addresses the IPA, extracts its metadata (bundle
id, version, provisioning-profile expiry, enrolled devices), generates the
OTA `manifest.plist` iOS expects, and serves a one-tap install page — a
registered iPhone installs the latest build from mobile Safari with no
cable, no Mac, no TestFlight.

The full pipeline is: **build → upload → install/share → (occasionally)
register a new device**.

## Auth

| Surface | Gate |
|---|---|
| Admin UI (`/`, `/b/{id}`, `/app/…`, `/shares`) | session login with the shared `PASSWORD` |
| JSON API (`/api/…`) | `X-API-Key: <key>` or `Authorization: Bearer <key>` |
| Install payload (`/i/{token}/…`), share pages (`/s/{token}`), register pages (`/register/{token}`) | high-entropy capability tokens, no auth |

The API accepts the env key `SIDELOAD_API_KEY` **or** a key minted by
`https://keys.farfield.systems` (`ffk_…`, scope **write**, app `sideload`
or `*`). Prefer a minted key — it's scoped, expiring, and revokes
instantly. Getting a key on this machine, in order of preference:

**1. Mint one yourself (preferred — works even when `.env` is stale).**
The keys app has no key-gated API, but its session UI is plain form POSTs,
fully scriptable. Log in with the fleet `PASSWORD` (reliably present in
`~/Developer/farfield/.env` even when that file predates newer apps), then
create the key; the `ffk_…` token appears exactly once in the response
HTML. Keep the secret out of the transcript, and do the whole flow in one
shell invocation (agent Bash calls don't share state) parking the token in
a scratch file:

```sh
JAR="$SCRATCH/cookies.txt"
PW=$(grep '^PASSWORD=' ~/Developer/farfield/.env | cut -d= -f2-)
curl -sS -o /dev/null -A "farfield-agent" -c "$JAR" \
  --data-urlencode "password=$PW" https://keys.farfield.systems/login
curl -sS -A "farfield-agent" -b "$JAR" \
  --data-urlencode name=agent-sideload --data-urlencode app=sideload \
  --data-urlencode scope=write --data-urlencode expires_days=30 \
  https://keys.farfield.systems/keys \
  | grep -o 'ffk_[A-Za-z0-9_-]*' | head -1 > "$SCRATCH/sideload.key"
chmod 600 "$SCRATCH/sideload.key"
# later calls: KEY=$(<"$SCRATCH/sideload.key")
```

A successful login 303s to `/`; a failure 303s to `/login?error=…` and is
rate-limited per client IP — verify the redirect, don't retry blind.

**2. Dev repo `.env`**: `KEY=$(grep '^SIDELOAD_API_KEY=' ~/Developer/farfield/.env | cut -d= -f2-)`.
Caveat: that file can be stale — it may predate the sideload app and lack
the key entirely (only `PASSWORD` is reliably current). A missing key here
means "mint one", not "the service is broken".

**3. Production `.env`**: `ssh iam@homelab.local`,
`~/projects/farfield/.env`. Last resort — agent sessions often can't get
ssh past the permission gate, and minting makes it unnecessary.

**Cloudflare edge caveats** (applies to every `*.farfield.systems` call):
the edge 403s bot User-Agents — always send a real-looking `-A` / UA — and
caps request bodies around 100 MB. An IPA larger than that cannot go
through the tunnel; upload it from the homelab itself (the service is
reachable on its port there).

## 1. Build the IPA

Archive for generic iOS, then export with method `debugging` (Xcode 15+
name for a development-signed export; `release-testing` = ad-hoc also
works). Both profile types embed the registered device list — only devices
in the profile can install.

```sh
xcodebuild archive \
  -scheme MyApp -destination 'generic/platform=iOS' \
  -archivePath build/MyApp.xcarchive \
  -allowProvisioningUpdates

cat > build/export.plist <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>method</key><string>debugging</string>
  <key>signingStyle</key><string>automatic</string>
</dict></plist>
EOF

xcodebuild -exportArchive \
  -archivePath build/MyApp.xcarchive \
  -exportOptionsPlist build/export.plist \
  -exportPath build/export \
  -allowProvisioningUpdates
# → build/export/MyApp.ipa
```

## 2. Upload

`POST /api/builds` — multipart field `ipa`, or the raw IPA as the request
body with `?filename=`. Optional query params `commit` and `notes` are
stored with the build.

```sh
curl -sS -A "farfield-agent" \
  -H "X-API-Key: $KEY" \
  -F ipa=@build/export/MyApp.ipa \
  "https://sideload.farfield.systems/api/builds?commit=$(git rev-parse --short HEAD)&notes=nightly"
```

Response (`201`):

```jsonc
{ "id", "cid", "bundleId", "appName", "version", "buildNumber",
  "profileExpiry",        // RFC3339 — from the embedded provisioning profile
  "deviceCount",          // devices enrolled in that profile
  "sizeBytes",
  "installURL" }          // https://sideload.farfield.systems/b/{id}
```

Uploading the same IPA twice is fine — builds are content-addressed by CID.
Watch `profileExpiry`: the UI warns under 14 days; an expired profile means
installs stop launching and the app must be re-archived and re-uploaded.

## 3. Install and share

- **`installURL` (`/b/{id}`) is session-gated** — it's the owner's build
  page, opened in Safari on their own phone after logging in. The actual
  install button is an `itms-services://` link pointing at
  `/i/{token}/manifest.plist` (token-gated, no cookie needed by the iOS
  install daemon).
- **For anyone else, mint a share link** — a public, expiring,
  install-count-limited landing page:

```sh
curl -sS -A "farfield-agent" -X POST -H "X-API-Key: $KEY" \
  "https://sideload.farfield.systems/api/builds/$ID/share?ttl=24h&max=3&label=for-alice"
# → { "token", "shareURL": ".../s/{token}", "expiresAt", "maxInstalls" }
```

`ttl`: `30m` (default) | `2h` | `24h`. `max`: `1` (default) | `3` |
`unlimited` (or any integer; `0` = unlimited). Revoking is UI-only
(`/shares`). The recipient must open the share URL **in Safari on the
device** — other browsers and the camera-app preview won't trigger
`itms-services:`.

Other API management calls:

| Call | Does |
|---|---|
| `GET /api/builds` | list every build |
| `DELETE /api/builds/{id}` | delete one build + its IPA blob |
| `DELETE /api/apps/{bundle}` | delete an app entirely (all versions) |

## 4. Device registration (when an install fails with "unable to install")

That error almost always means the device's UDID is not in the embedded
provisioning profile. The fix is a loop through Apple's portal:

1. In the sideload admin UI, open the app page → **devices** → enable
   registration, and send the `/register/{token}` link to the device.
2. The device opens it in Safari and installs `enroll.mobileconfig`; Apple
   posts the device's UDID back and sideload captures it. The device now
   shows as **pending** (registered with sideload, absent from the latest
   build's profile).
3. Add the UDID at developer.apple.com (Devices), regenerate/refresh the
   provisioning profile (`-allowProvisioningUpdates` picks it up on the
   next archive).
4. Re-archive, re-export, re-upload. Pending count drops to zero when the
   new build's profile contains the device.

`GET /app/{bundle}/devices.txt` (session) exports the UDID list for bulk
upload to the portal. Registration enable/disable and device management are
session-gated UI only — there is no device API.

## Verify

`GET /status` → `{ "service": "sideload", "ok": true, "builds": n }` —
public, no key needed.
