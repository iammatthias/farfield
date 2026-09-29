# WebAssembly in farfield

farfield already runs WebAssembly in three places, each for a different reason:

| Where | What | Why WebAssembly |
|---|---|---|
| `lib/editor` | The text editor: document, Markdown, font rasterizer, layout, renderer, undo. Hand-written `.wat`, assembled by `lib/wat` | One engine that runs identically in a browser (on a canvas) and on the desktop (`apps/desk`, under wazero). The page supplies input and a surface, and nothing else |
| `apps/blobs` | HEIC → JPEG, via libheif compiled to WebAssembly and run by wazero | A C decoder without cgo, so every binary stays static on distroless |
| `apps/content` | Ternlight embeddings for fleet search, on the reader's device | Search without a model server |

The pattern that works: **use WebAssembly where one piece of logic must run in more than one place, or where native code would otherwise need cgo.** Don't reach for it where the browser or Go already does the job well.

## The editor, in brief

- **Source:** `lib/editor/wat/*.wat` is the source of truth. It's written by hand, instruction by instruction.
  - `mem.wat` — memory map
  - `text.wat` — buffer and UTF-8
  - `font.wat` — TrueType reader and rasterizer
  - `markdown.wat`
  - `layout.wat`
  - `render.wat`
  - `edit.wat` — the host API
- **Assembly:** `lib/wat` assembles the text format in pure Go at server start (milliseconds), so no binary is committed to drift from its source.
- **Browser host:** `host.js` owns a `<canvas>` and one invisible `<textarea>`, which is the only way a browser delivers IME, dictation and phone keyboards. `mount.js` turns any `<textarea data-editor>` into the editor and keeps the original as the posting field.
- **Desktop host:** `apps/desk` runs the same module natively. Ebitengine provides the window, with no cgo on macOS.
- **Fonts:** Newsreader and IBM Plex Mono as static TTF (OFL). The editor rasterizes them itself, and a test checks the rasterizer against `x/image/vector` to within ~0.3% of ink.
- **Size:** the whole engine is about 20 KB of wasm. The fonts are the weight, at about 880 KB of TTF, cached for a year.

## Opportunities, ranked

### 1. Sanitize photos before they leave the device

**Where:** blobs upload paths (feed and content editors, library covers)

`apps/blobs/sanitize.go` strips GPS and EXIF and converts HEIC server-side, after the bytes have crossed the network. The same work in the browser would:

- **Privacy:** location data never leaves the phone.
- **Uplink:** a JPEG is usually a third the size of the HEIC original's upload.
- **Homelab CPU:** no decode work on the box.

The pieces exist: libheif-wasm is what blobs already runs. The browser side is a small module doing EXIF strip and orientation (the JPEG segment walk from `sanitize.go` ports directly) plus HEIC decode. **Keep the server pass regardless:** the browser can't be the only line of defence for a public, edge-cached blob.

### 2. One Markdown engine for the editor, the preview and the site

**Where:** `lib/markdown`, the editor's `markdown.wat`, the site's Astro renderer

There are three Markdown implementations today:

- the Go renderer the apps and site read from
- the editor's live styler
- whatever the site does at build time

Blob and series references, footnotes and hard-wrap rules drift between them. A single parser module, called from Go via wazero and from the browser directly, would make "what the editor shows" and "what the site renders" provably the same.

Start with block and inline classification, which the editor already has in hand-written form, and grow it into an AST that `lib/markdown` walks for HTML.

### 3. Sandboxed tools for the texting agent

**Where:** switchboard / `ff-agent`

The agent runs as a user with docker access; see the switchboard review. Deterministic capabilities could run as WebAssembly modules under wazero instead of as shell commands:

- recipe scaling (`lib/recipe`)
- QR generation (`lib/qrenc`)
- image resize
- feed and scrap formatting

The imports are then the whole capability surface: no filesystem, no network except what the host function hands in. That's a real security boundary for anything a prompt injection can reach, which the current shell doesn't have.

### 4. A reading surface for library

**Where:** `apps/library`

The library admin lists EPUBs but can't open them. The editor already has everything a reader needs:

- font rasterization at any size
- a real layout engine with a reading measure
- selection
- light and dark palettes

A read-only mode fed EPUB chapter text would give an in-browser reader with typography that matches the rest of farfield. It would also work on the desktop via `apps/desk`.

### 5. Instant QR preview

**Where:** `apps/qr`

The code form round-trips to the server to see a code. `lib/qrenc` is small and pure. As a module, the form could draw the code on every keystroke (error-correction level, target, proxy vs direct) and post only to save.

This is low effort, and a good second consumer for `lib/wat`.

### 6. Blob identity in the browser

**Where:** blobs and editor uploads

Hashing a file to its CID before upload lets a client skip uploading bytes blobs already has. That matters over a 34 Mbps home uplink. **Use WebCrypto's SHA-256 here, not WebAssembly:** the browser already does it natively and fast. Listed so it doesn't get built the wrong way.

## Where not to use it

- **pulse charts, daily art, wordle and sudoku:** these are canvas or DOM drawing with modest logic, and JavaScript is the right tool. daily art already leans on three.js for the GPU.
- **Server-side Go:** nothing to gain from compiling Go apps to WebAssembly. They run natively.
- **Hashing, crypto, compression in the browser:** use the platform's built-ins (WebCrypto, CompressionStream).
