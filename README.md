# farfield

The content backend for [iammatthias.com](https://iammatthias.com) — a set of
small, single-binary Go services. Each one is an HTML admin UI for writing and
moderating content, plus a public JSON API the website reads.

| Service   | Address                    | Holds                                          |
|-----------|----------------------------|------------------------------------------------|
| `apex`    | `farfield.systems`         | the landing page — a standalone SVG            |
| `content` | `content.farfield.systems` | collections, entries, series fragments         |
| `feed`    | `feed.farfield.systems`    | ephemeral short-form posts                     |
| `blobs`   | `blobs.farfield.systems`   | image/media bytes + metadata, on Cloudflare R2 |
| `calendar`| `calendar.farfield.systems`| daily photo calendar: NASA APOD + UFO easter egg |
| `bookmarks`| `bookmarks.farfield.systems`| curated public/private link list with OG metadata |
| `qr`      | `qr.farfield.systems`      | direct and editable-proxy QR codes              |
| `backup`  | tailnet-only               | snapshots every app's database into R2         |

## Stack

The standard library plus one dependency: `modernc.org/sqlite`, a pure-Go
SQLite driver — no cgo, so every build is a static binary. HTTP is `net/http`,
templates are `html/template`, assets are embedded with `embed`, logging is
`log/slog`. No web framework, no ORM, no router library.

That shape — standard library first, one deliberate dependency — is a Go
adaptation of [andromeda](https://github.com/stevedylandev/andromeda), the
standard-library-first Rust stack it started from.

Three ideas run through every app:

- **Content-addressed.** Every record carries a CID — a sha-256 hash of its
  content — for verification, change detection, and ETag caching. See the
  `content-addressing` skill.
- **Self-migrating.** A database migrates its own schema when it is opened;
  deploying new code *is* the migration step. See the `self-migrating-sqlite`
  skill.
- **One aesthetic.** The admin UIs share a Braun × JPL × vintage-NASA
  instrument-panel look. See the `farfield-style` skill.

## Layout

```
go.work          Workspace — joins every module below.
lib/auth         Password verify, session tokens, cookies.
lib/store        Env loading, short-IDs, sessions table.
lib/theme        Shared CSS + editor JS, embedded.
lib/cid          Content-addressed identifiers (CIDv1).
lib/backup       Snapshot + content-addressed push/pull.
apps/*           One module per deployable service.
docs/API.md      The website-facing API contract.
```

The `lib/*` modules have zero dependencies; only apps pull in the SQLite driver.

## Development

The workspace root is not itself a module, so `./...` does not span it. Build,
vet, and test every module the way CI does — by iterating the workspace:

```sh
for dir in $(go list -m -f '{{.Dir}}'); do
	(cd "$dir" && go build ./... && go vet ./... && go test ./...)
done
```

Run one app directly:

```sh
go run ./apps/content
```

Every app reads a single `.env` at the repo root — copy `.env.example` to start.

## Deployment

`docker compose` builds each app from source into a `distroless/static` image.
Production runs on a homelab server, exposed publicly through a Cloudflare
tunnel.

## Reference

- [`docs/API.md`](docs/API.md) — the JSON API the website consumes.
- [`.claude/skills/`](.claude/skills/) — `farfield-stack` (scaffold a new app),
  `farfield-style` (the visual system), `content-addressing`, and
  `self-migrating-sqlite`.
