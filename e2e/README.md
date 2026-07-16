# farfield e2e

Browser-level checks for the content editor — the round-trip invariant
(open → save never corrupts stored markdown), rich-edit serialization, and
autosave.

```sh
make dev                      # fleet on localhost, password "demo"
npm i --no-save playwright    # once; then: npx playwright install chromium
make e2e
```

Playwright is deliberately not committed as a dependency — farfield has no
node toolchain. The suite is one file (content-editor.mjs) that seeds its own
entry over the write API and deletes it afterward.
