# farfield

A Go monorepo for small, single-binary web applications.

The stack is deliberately the standard library plus one dependency
(`modernc.org/sqlite`, a pure-Go SQLite driver — no cgo, so every build is a
static binary). HTTP is `net/http`, templates are `html/template`, assets are
embedded with `embed`, logging is `log/slog`.

## Layout

```
go.work          Workspace — joins every module below.
lib/auth         Password verify, session tokens, session cookies.
lib/store        Env loading, short-IDs, sessions table helpers.
lib/theme        Shared CSS, embedded.
apps/*           One module per deployable app.
```

The `lib/*` modules have zero dependencies. Only apps pull in the SQLite
driver.

## Development

Everything builds as one workspace:

```sh
go build ./...
go vet ./...
go test ./...
```

## Creating an app

The full scaffold pattern — structure, code patterns, auth, Docker, and the
wiring checklist — lives in
[`.claude/skills/farfield-stack/SKILL.md`](.claude/skills/farfield-stack/SKILL.md).
