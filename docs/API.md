# farfield API

farfield is the content backend for `iammatthias.com`. It is a set of small
single-binary services; the website reads three of them.

| Service   | URL                                  | Holds                                   |
|-----------|--------------------------------------|-----------------------------------------|
| content   | `https://content.farfield.systems`   | collections, entries, series fragments  |
| feed      | `https://feed.farfield.systems`      | short ephemeral posts                   |
| blobs     | `https://blobs.farfield.systems`     | image/media bytes + metadata            |
| apex      | `https://farfield.systems`           | the standalone landing page (not an API)|

The `backup` service is internal (tailnet-only) and has no public API.

## Conventions

- **Reads are public** — no auth — and send `Access-Control-Allow-Origin: *`,
  so the browser can fetch them directly.
- **Writes need a key** — `X-API-Key: <key>` (or `Authorization: Bearer <key>`).
- **Keys vs CIDs.** Every record has a stable **key** (`slug`, `rkey`, or
  `id`) and a **CID** — a CIDv1 (sha-256) hash of its *content*. The key never
  changes; the CID changes whenever the content does. Use the CID for
  change-detection, cache validation, or verification (re-hash to confirm).
- **Conditional GET.** Single-record endpoints send the CID as a strong
  `ETag`. Send `If-None-Match: "<cid>"` to get `304 Not Modified` when
  unchanged. Blob bytes are immutable and cached forever.
- Timestamps are RFC3339 UTC strings.
- The public API returns **published entries only**; drafts never appear.

## content — `https://content.farfield.systems`

| Method & path                       | Returns                                  |
|--------------------------------------|------------------------------------------|
| `GET /api/collections`               | `{ "collections": [Collection, …] }`     |
| `GET /api/entries[?collection=slug]` | `{ "entries": [Entry, …] }` — published  |
| `GET /api/entries/{slug}`            | `Entry` — `404` if missing/draft; `ETag` |
| `GET /api/series`                    | `{ "series": [Series, …] }`              |
| `GET /api/series/{rkey}`             | `Series` — `404` if missing; `ETag`      |
| `GET /status`                        | `{ "service", "ok", "collections" }`     |
| `POST /api/entries`                  | create — `X-API-Key`                     |
| `PUT /api/entries/{slug}`            | replace — `X-API-Key`                    |
| `DELETE /api/entries/{slug}`         | delete — `X-API-Key`                     |

Collections and series are managed in the admin UI; only entries have a
write API.

## feed — `https://feed.farfield.systems`

| Method & path             | Returns                              |
|---------------------------|--------------------------------------|
| `GET /api/posts`          | `{ "posts": [Post, …] }`             |
| `GET /api/posts/{id}`     | `Post` — `404` if missing; `ETag`    |
| `GET /status`             | `{ "service", "ok", "posts" }`       |
| `POST /api/posts`         | create — `X-API-Key`                 |
| `PUT /api/posts/{id}`     | replace — `X-API-Key`                |
| `DELETE /api/posts/{id}`  | delete — `X-API-Key`                 |

## blobs — `https://blobs.farfield.systems`

| Method & path             | Returns                                            |
|---------------------------|----------------------------------------------------|
| `GET /blobs/{cid}`        | raw bytes — `ETag`, `Cache-Control: …immutable`    |
| `GET /blobs/{cid}/meta`   | `BlobMeta`                                         |
| `GET /blobs[?page=N]`     | `{ "blobs": [BlobMeta, …], "total", "page", "pages" }` |
| `GET /status`             | `{ "service", "ok", "blobs" }`                     |
| `POST /blobs`             | upload bytes → `BlobMeta` — `X-API-Key`            |
| `DELETE /blobs/{cid}`     | delete — `X-API-Key`                               |

A blob's `cid` *is* the hash of its bytes, so `/blobs/{cid}` is immutable —
cache it forever.

## Record shapes

```jsonc
// Collection
{ "slug", "name", "description"?, "createdAt", "entryCount" }

// Entry — body is markdown; see "Body URIs" below
{ "collection", "slug", "cid", "title", "excerpt"?, "body",
  "tags": [], "published", "createdAt", "updatedAt" }

// Series — a reusable markdown fragment
{ "rkey", "cid", "title"?, "body", "createdAt", "updatedAt" }

// Post — feed
{ "id", "cid", "body", "tags": [], "createdAt", "updatedAt" }

// BlobMeta
{ "cid", "size", "mime", "width"?, "height"?, "blurhash"?, "dominantColor"? }
```

## Body URIs

Entry and post `body` markdown embeds two custom URIs. Resolve them before
rendering:

- **`blob://<cid>`** — rewrite to `https://blobs.farfield.systems/blobs/<cid>`.
  For dimensions / blurhash / a blur-up placeholder, read `GET /blobs/<cid>/meta`.
- **`![](series://<rkey>)`** — fetch `GET /api/series/<rkey>` and splice the
  fragment's `body` markdown in place of the whole image construct. The
  fragment itself contains `blob://` images.

## Client module

Drop this into the Astro project as `src/lib/farfield.ts`:

```ts
// farfield backend client for iammatthias.com
const CONTENT = "https://content.farfield.systems";
const FEED    = "https://feed.farfield.systems";
const BLOBS   = "https://blobs.farfield.systems";

export type Entry = {
  collection: string; slug: string; cid: string; title: string;
  excerpt?: string; body: string; tags: string[]; published: boolean;
  createdAt: string; updatedAt: string;
};
export type Collection = {
  slug: string; name: string; description?: string;
  createdAt: string; entryCount: number;
};
export type Series = {
  rkey: string; cid: string; title?: string; body: string;
  createdAt: string; updatedAt: string;
};
export type Post = {
  id: string; cid: string; body: string; tags: string[];
  createdAt: string; updatedAt: string;
};
export type BlobMeta = {
  cid: string; size: number; mime: string;
  width?: number; height?: number; blurhash?: string; dominantColor?: string;
};

const json = async (r: Response) => (r.ok ? r.json() : Promise.reject(r.status));

// ── content ────────────────────────────────────────────────────────────────
export const getCollections = (): Promise<Collection[]> =>
  fetch(`${CONTENT}/api/collections`).then(json).then(d => d.collections);

export const getEntries = (collection?: string): Promise<Entry[]> =>
  fetch(`${CONTENT}/api/entries${collection ? `?collection=${collection}` : ""}`)
    .then(json).then(d => d.entries);

export const getEntry = async (slug: string): Promise<Entry | null> => {
  const r = await fetch(`${CONTENT}/api/entries/${slug}`);
  return r.ok ? r.json() : null;          // 404 (draft/missing) → null
};

export const getSeries = async (rkey: string): Promise<Series | null> => {
  const r = await fetch(`${CONTENT}/api/series/${rkey}`);
  return r.ok ? r.json() : null;
};

// ── feed ───────────────────────────────────────────────────────────────────
export const getPosts = (): Promise<Post[]> =>
  fetch(`${FEED}/api/posts`).then(json).then(d => d.posts);

// ── blobs ──────────────────────────────────────────────────────────────────
export const blobURL = (cid: string) => `${BLOBS}/blobs/${cid}`;
export const getBlobMeta = async (cid: string): Promise<BlobMeta | null> => {
  const r = await fetch(`${BLOBS}/blobs/${cid}/meta`);
  return r.ok ? r.json() : null;
};

// ── body resolution ────────────────────────────────────────────────────────
// Resolve a body's farfield URIs before handing it to a markdown renderer:
//   ![](series://<rkey>)  → replaced by the fragment's own markdown
//   blob://<cid>          → rewritten to the blobs URL
async function replaceAsync(
  s: string, re: RegExp, fn: (m: string, ...g: string[]) => Promise<string>,
): Promise<string> {
  const jobs: Promise<string>[] = [];
  s.replace(re, (m, ...g) => { jobs.push(fn(m, ...g)); return m; });
  const done = await Promise.all(jobs);
  return s.replace(re, () => done.shift()!);
}

export async function resolveBody(markdown: string): Promise<string> {
  // 1. splice series fragments in (the whole ![](series://rkey) → the fragment)
  const spliced = await replaceAsync(
    markdown, /!\[[^\]]*\]\(series:\/\/([a-z0-9]+)\)/g,
    async (_m, rkey) => (await getSeries(rkey))?.body ?? "",
  );
  // 2. rewrite blob:// image URLs (in the entry and the spliced-in fragments)
  return spliced.replace(/blob:\/\/([a-z0-9]+)/g, (_m, cid) => blobURL(cid));
}
```

**Usage:** `const html = render(await resolveBody(entry.body))`. `getBlobMeta`
gives `width`/`height`/`blurhash` for blur-up placeholders. The per-record
`cid` is a content fingerprint — use it as a cache key, send it as
`If-None-Match`, or compare across builds to detect what changed.

## Migrating from the old records API

The old records-engine API is gone. Anything hitting it must change:

- `GET /records/{collection}` / `/records/{collection}/{rkey}` →
  `GET /api/entries?collection=` / `GET /api/entries/{slug}`
- `GET /schemas` → removed (no schema endpoint)
- `?since={cursor}` incremental sync → removed; refetch in full
- The `{ collection, rkey, cid, value: {…} }` envelope → responses are flat;
  `rkey` is now `slug`
- Drafts are filtered server-side — drop any `published === true` checks
- feed posts no longer have a `link` field — links go inline in the markdown
