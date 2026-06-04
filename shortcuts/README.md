# farfield · iOS Shortcuts

Three small iOS Shortcuts that publish to the farfield apps, written in
[Cherri](https://github.com/electrikmilk/cherri) and compiled to signed
`.shortcut` files.

| Shortcut | Input | What it does |
|---|---|---|
| `bookmarks` | a URL | saves it as a public bookmark; server fetches OG metadata |
| `feed` | text (or share-sheet input pre-filled) | publishes a feed post |
| `qr` | a URL | creates a public, enabled, proxy-mode QR; copies the SVG URL |

Each shortcut is share-sheet enabled and prompts for nothing it can avoid.
Refine titles, categories, visibility, etc. in the admin UIs.

## Build

macOS only (Apple's `shortcuts sign` is macOS-only).

```sh
brew install electrikmilk/cherri/cherri
cd shortcuts
./build.sh                 # signed, share=contacts (default)
./build.sh anyone          # signed, share=anyone
```

Each `.cherri` → one signed `*.shortcut` beside it. Open it on the Mac
(`open *.shortcut`) — Shortcuts prompts for the `#question` values once
at import (API key, base URL, default category), and iCloud syncs the
shortcut to every signed-in device.

`.shortcut` outputs start with `AEA1` (Apple Encrypted Archive) and import
without an "untrusted shortcut" warning.

## Install-time prompts (`#question`)

| Shortcut | Asks for |
|---|---|
| `bookmarks` | `apiKey` (BOOKMARKS_API_KEY), `baseURL`, `defCat` |
| `feed` | `apiKey` (FEED_API_KEY), `baseURL` |
| `qr` | `apiKey` (QR_API_KEY), `baseURL` |

API keys come from the deployment's `.env` — same values the apps load via
`store.Env(...)` on the server (see `docker-compose.yml`).
