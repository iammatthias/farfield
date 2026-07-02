---
name: Farfield Stack
description: Scaffold a Go CRUD web application in the farfield go.work monorepo using net/http, modernc.org/sqlite, html/template, embedded assets, session or API-key auth, and Docker. Use when the user wants to build a new Go web server with CRUD operations in the farfield workspace.
---

# Farfield Go Web App

## Overview

Scaffold and build Go CRUD web applications in the farfield workspace using
the **standard library** plus exactly one required external dependency
(`modernc.org/sqlite`). The result is a single static binary web server with
HTML pages, a JSON API, session/API-key auth, and Docker deployment.

The stack, deliberately, is almost entirely `std`:

| Concern        | Tool                                            |
|----------------|-------------------------------------------------|
| HTTP server    | `net/http` via `web.Serve` (`lib/web`)          |
| Routing        | `http.ServeMux` patterns (`GET /items/{id}`)    |
| Database       | `database/sql` + `modernc.org/sqlite` via `store.OpenDB` |
| Templates      | `html/template` via `web.ParseTemplates` + `web.Renderer` |
| Static assets  | `embed` + `http.FileServerFS`                   |
| JSON           | `web.WriteJSON` / `web.WriteError` / `web.WriteRecord` |
| Auth           | `web.Auth` (sessions, API keys, read keys, admin-issued keys) |
| Middleware     | `web.LogRequests`, `web.CORS`, `web.Gzip`, `web.RateLimit`, `web.FailLimit` |
| Telemetry      | `lib/pulse` (`pulse.New` → `Wrap` → `Close`)    |
| Logging        | `log/slog`                                      |

**`modernc.org/sqlite` is the only required external dependency** — a pure-Go
SQLite driver, no cgo, so every build is `CGO_ENABLED=0` and produces a static
binary that runs on distroless.

Apps live under `apps/`. Shared libraries live under `lib/` as their own
modules, joined into one build by a root `go.work` workspace:

- `lib/web` — **the HTTP plumbing every app uses**: `Serve` (graceful
  shutdown), `Health` (docker healthcheck probe), `Auth` gates, middleware,
  JSON writers, `RateLimiter`/`FailLimiter`, template parsing/rendering
- `lib/store` — `OpenDB` (standard pragmas: WAL, busy_timeout,
  synchronous=NORMAL, FKs), env loader, `ShortID`, sessions table helpers,
  `EnsureColumn`/`RenameColumn` migrations
- `lib/auth` — password/API-key verification, session tokens, cookies
- `lib/theme` — shared CSS + editor JS (`theme.CSSHandler`, `theme.Version`)
- `lib/cid` — content-addressed identifiers (`Of`, `OfValue`, `OfReader`,
  `Valid`, `WellFormed`)
- `lib/pulse` — per-request traffic recording into the app's own SQLite
- `lib/keys` — admin-issued, scoped API keys (shared `keys.sqlite`; minted by
  the `keys` app; `keys.Attach(s.auth, "<app>")` wires them in)
- `lib/qrenc` — QR encoder → SVG (used by qr, sideload)
- `lib/backup` — snapshot helpers for the backup app

**Companion skills.** `content-addressing` covers the `cid` every record
carries. `self-migrating-sqlite` covers schema migration in `openDB`.
`farfield-style` covers the shared visual system. `farfield-deploy` covers
getting it onto the homelab.

## Project Structure

```
farfield/
├── go.work                      # workspace — joins every module
├── go.work.sum
├── .env.example                 # one env template — every app reads the root .env
├── Dockerfile                   # ONE Dockerfile for every app (APP build arg)
├── docker-compose.yml           # production compose — all apps, built from source
├── lib/…                        # shared modules (see above)
├── apps/
│   └── app-name/
│       ├── go.mod               # module .../apps/app-name
│       ├── main.go              # entry point — env, health subcommand, run()
│       ├── server.go            # embed, Server struct, run, routes, handlers
│       ├── db.go                # schema, model, CRUD functions
│       ├── auth.go              # only for app-specific gates beyond web.Auth
│       └── templates/           # base.html + pages (embedded)
└── .github/workflows/
    ├── ci.yml                   # build/vet/test every module — no per-app list
    └── docker.yml               # ghcr image builds — per-app matrix
```

There is **no per-app Dockerfile and no per-app docker-compose.yml** — the
root `Dockerfile` builds any app via the `APP` build arg, and the root
compose builds every service from source on the host.

## The workspace (go.work)

The root `go.work` lists every module. `./...` from the bare workspace root
does **not** span modules — iterate `go list -m` instead (exactly what CI
does):

```sh
for dir in $(go list -m -f '{{.Dir}}'); do
	(cd "$dir" && go build ./... && go vet ./... && go test ./...)
done
```

Each app's `go.mod` requires the libs it uses at `v0.0.0` **and** carries a
`replace` for each (`github.com/iammatthias/farfield/lib/web => ../../lib/web`).
The `replace` block is **not optional** — without it, per-module and Docker
builds fail looking for a published `v0.0.0`. Transitive lib deps need the
replace too (e.g. `lib/theme` pulls `lib/cid`). Run `go work sync` after
editing, never per-module `go mod tidy`.

The lib modules stay dependency-free (driver-free); each app imports
`_ "modernc.org/sqlite"` itself.

## Database Layer (db.go)

One file per app, raw `database/sql`, standalone CRUD functions taking
`*sql.DB` first. No ORM. Open through the shared helper so every app gets the
same pragmas and pool:

```go
func openDB(path string) (*sql.DB, error) {
	db, err := store.OpenDB(path) // WAL, busy_timeout, synchronous=NORMAL, FKs
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// Session auth: also run db.Exec(store.SessionSchema).
	// Evolving schema: store.EnsureColumn / store.RenameColumn + backfills —
	// see the self-migrating-sqlite skill.
	return db, nil
}
```

Key patterns: `store.ShortID()` for record keys; `?` placeholders always;
`sql.ErrNoRows` → `(nil, nil)` for get-by-id; `store.NowRFC3339()` for
timestamps; records carry a `cid` via `cid.OfValue`.

## Server Layer (server.go)

```go
//go:embed templates
var assets embed.FS

type Server struct {
	db   *sql.DB
	auth *web.Auth
	rd   *web.Renderer
	// pulse records request telemetry; nil disables it (tests never start it).
	pulse *pulse.Recorder
}

func run(host, port string) error {
	db, err := openDB(store.Env("APP_DB_PATH", "app.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.PruneSessions(db); err != nil {
		slog.Warn("could not prune sessions", "err", err)
	}

	tmpl, err := web.ParseTemplates(assets, nil) // templates/*.html over base.html
	if err != nil {
		return err
	}

	s := &Server{
		db: db,
		auth: &web.Auth{
			DB:           db,
			Password:     store.Env("PASSWORD", ""),
			APIKey:       store.Env("APP_API_KEY", ""),
			ReadKey:      store.Env("APP_READ_KEY", ""),
			CookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
		},
		rd: &web.Renderer{Templates: tmpl, AssetVer: theme.Version},
	}

	defer keys.Attach(s.auth, "app-name")() // admin-issued keys, when KEYS_DB_PATH is set

	s.pulse = pulse.New(s.db, "app-name")
	defer s.pulse.Close()
	return web.Serve(host, port, s.routes()) // graceful shutdown built in
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// HTML admin UI — session-gated.
	mux.HandleFunc("GET /{$}", s.auth.RequireSession(s.handleIndex))

	// Login (wrap POST /login in web.FailLimit for sensitive apps).
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.auth.HandleLogin)
	mux.HandleFunc("GET /logout", s.auth.HandleLogout)

	// JSON API. Reads: RequireReadKey (open until <APP>_READ_KEY is set).
	// Public single-record reads: web.RateLimit(s.rl, s.auth.HasReadKey, h).
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /api/items", s.auth.RequireReadKey(s.handleAPIList))
	mux.HandleFunc("POST /api/items", s.auth.RequireAPIKey(s.handleAPICreate))

	// Shared theme stylesheet.
	mux.HandleFunc("GET /static/styles.css", theme.CSSHandler())

	// CORS only for apps with a browser-facing API; skip Gzip for raw-bytes
	// routes (blobs/library); pulse innermost so logged timings stay real.
	return web.CORS(web.LogRequests(web.Gzip(s.pulse.Wrap(mux))),
		"GET", "POST", "PUT", "DELETE", "OPTIONS")
}
```

Handlers render with `s.rd.Render(w, "page.html", map[string]any{...})` and
answer JSON with `web.WriteJSON` / `web.WriteError`; single records go
through `web.WriteRecord(w, r, record.CID, record)` for ETag/304 handling.

## Auth

`web.Auth` provides every gate:

- `RequireSession` — HTML admin routes (sessions table via `lib/store`).
- `RequireAPIKey` — writes; fails **closed** (503) when nothing is configured.
- `RequireReadKey` — reads; deliberately fail-**open** until `<APP>_READ_KEY`
  is set.
- `HasReadKey` / `HasWriteKey` — predicates for rate-limit exemptions and
  draft previews.
- Keys arrive as `X-API-Key: <key>` or `Authorization: Bearer <key>`.

On top of the env keys, `keys.Attach(s.auth, "<app>")` (no-op unless
`KEYS_DB_PATH` is set) makes the same gates honor **admin-issued keys** —
scoped read/upload/write per app, minted and revoked in the `keys` app, with
instant effect. App-specific credentials beyond that (e.g. the library's
upload gate) live in the app's own `auth.go` and should also consult
`s.auth.Keys` for the matching scope.

## main.go

```go
func main() {
	_ = store.LoadEnv() // finds the root .env, wherever the app is run from
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// "health" probes the running server's /status for Docker healthchecks
	// (distroless: no shell, no curl).
	if len(os.Args) > 1 && os.Args[1] == "health" {
		os.Exit(web.Health(store.Env("APP_PORT", "3000")))
	}

	host := store.Env("HOST", "127.0.0.1")
	port := store.Env("APP_PORT", "3000")
	if err := run(host, port); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
```

Genuinely global env values — `HOST`, `PASSWORD`, `COOKIE_SECURE` — are
unprefixed; everything else is app-prefixed **including the port**. Append
new variables to the root `.env.example`; never create per-app env files.

## Templates

`templates/base.html` defines `{{define "base"}}` with `title`/`content`
blocks; pages redefine them. `web.Renderer` injects `.AssetVer` so the shared
stylesheet URL (`/static/styles.css?v={{.AssetVer}}`) busts caches on theme
changes. `html/template` escapes by default — pass `template.HTML` only for
trusted, never user, content.

## Docker

One root `Dockerfile` for every app — compose passes `APP` per service:

```yaml
  app-name:
    <<: *defaults                    # restart, logging caps, healthcheck
    build:
      context: .
      args: { APP: app-name }
    image: farfield-app-name
    ports:
      - "${APP_PORT:-3000}:3000"
    environment:
      - HOST=0.0.0.0
      - APP_PORT=3000
      - APP_DB_PATH=/data/app-name.sqlite
      - PASSWORD=${PASSWORD:-changeme}
      - KEYS_DB_PATH=/data/keys.sqlite   # if the app has API keys
      - COOKIE_SECURE=true
    volumes:
      - ./data:/data
```

Every service shares the bind-mounted `./data` directory (host-side, survives
rebuilds); each app owns its own `<app>.sqlite` within it. The healthcheck
execs the binary's own `health` subcommand.

## Wiring up a new app — checklist

1. `mkdir -p apps/APP_NAME && cd apps/APP_NAME && go mod init github.com/iammatthias/farfield/apps/APP_NAME`
2. `go work use ./apps/APP_NAME` from the repo root
3. App `go.mod`: `require` the libs used (+ `modernc.org/sqlite`) **and
   matching `replace` directives** — including transitive ones (`lib/cid` via
   theme, `lib/auth` via web)
4. Write `db.go` (store.OpenDB + schema), `server.go` (Server, run, routes),
   `main.go` (env, health subcommand), `templates/`
5. Append the app's variables (app-prefixed) to the root `.env.example`
6. Add the app to `matrix.app` in `.github/workflows/docker.yml`
7. Add a service to the root `docker-compose.yml` (pattern above; pick the
   next free port)
8. If the app mints/accepts API keys: `keys.Attach` in `run()`, add the app
   to `knownApps` in `apps/keys/server.go`, and set `KEYS_DB_PATH` in compose
9. Add a docs page: `apps/apex/templates/docs/APP_NAME.html` + a `docPages`
   entry in `apps/apex/main.go`
10. `go work sync`, then verify like CI: from the app dir,
    `go build ./... && go vet ./... && go test ./...`
11. Expose it publicly by appending a Caddy block on the homelab
    (`<name>.farfield.systems`) — see the farfield-deploy skill

## Testing

Standard-library `testing` only. Co-locate `*_test.go`. `t.TempDir()` for a
throwaway SQLite file, `net/http/httptest` for handlers, `s.routes()` as the
handler under test. Tests never start pulse (leave `s.pulse` nil).

## What NOT to include

The point of this stack is the standard library. Do not add:

- **No web framework** — `net/http` has method routing and path wildcards (no gin, echo, chi, fiber)
- **No router library** — `http.ServeMux` is enough
- **No ORM or query builder** — raw `database/sql` (no gorm, ent, sqlx, sqlc)
- **No cgo SQLite** — `modernc.org/sqlite` keeps `CGO_ENABLED=0` (no mattn/go-sqlite3)
- **No template engine** — `html/template` (no templ, pongo2)
- **No config library** — env vars + `store.LoadEnv` (no viper, koanf)
- **No logging library** — `log/slog` (no zap, logrus, zerolog)
- **No UUID library** — `store.ShortID` / `crypto/rand` (no google/uuid)
- **No JWT library** — admin-issued keys are opaque tokens in `lib/keys`;
  revocation is a DB row, not a token lifetime

If a specific app genuinely needs another dependency, add it to that app's
`go.mod` alone — never to a `lib/*` module, and never as a workspace default.
