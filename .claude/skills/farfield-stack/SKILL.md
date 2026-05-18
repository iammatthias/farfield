---
name: Farfield Stack
description: Scaffold a Go CRUD web application in the farfield go.work monorepo using net/http, modernc.org/sqlite, html/template, embedded assets, session or API-key auth, and Docker. Use when the user wants to build a new Go web server with CRUD operations in the farfield workspace.
---

# Farfield Go Web App

## Overview

Scaffold and build Go CRUD web applications in the farfield workspace using the
**standard library** plus exactly one external dependency. The result is a
single static binary web server with HTML pages, a JSON API, optional session
or API-key auth, and Docker deployment.

The stack, deliberately, is almost entirely `std`:

| Concern        | Tool                                            |
|----------------|-------------------------------------------------|
| HTTP server    | `net/http` (`http.ServeMux`, method routing)    |
| Routing        | `http.ServeMux` patterns (`GET /items/{id}`)    |
| Database       | `database/sql` + `modernc.org/sqlite`           |
| Templates      | `html/template`                                 |
| Static assets  | `embed` + `http.FileServerFS`                   |
| JSON           | `encoding/json`                                  |
| Auth           | `crypto/subtle`, `crypto/rand`, `net/http` cookies |
| Logging        | `log/slog`                                       |
| Concurrency    | goroutines                                       |

**The only external dependency is `modernc.org/sqlite`** — a pure-Go SQLite
driver. It needs no cgo, so every build is `CGO_ENABLED=0` and produces a
static binary. There is no SQLite in the standard library; this is the one
unavoidable dependency, and it is chosen specifically to keep cgo out.

Apps live under `apps/`. Shared libraries live under `lib/` as their own
modules, joined into one build by a root `go.work` workspace:

- `lib/auth` — password verify, session token, cookie helpers (zero deps)
- `lib/store` — env loader, short-ID, session table helpers (zero deps)
- `lib/theme` — shared CSS + editor JS, embedded (zero deps)
- `lib/cid` — content-addressed identifiers (zero deps)

**Companion skills.** `content-addressing` covers the `cid` every record
carries — verification, ETags, the key-vs-CID distinction. `self-migrating-sqlite`
covers an `openDB` that migrates its own schema. `farfield-style` covers the
shared visual system. Reach for them when scaffolding.

## Project Structure

```
farfield/
├── go.work                      # workspace — joins every module
├── go.work.sum
├── .env.example                 # one env template — every app reads the root .env
├── lib/
│   ├── auth/
│   │   ├── go.mod               # module .../lib/auth — no dependencies
│   │   └── auth.go
│   ├── store/
│   │   ├── go.mod               # module .../lib/store — no dependencies
│   │   └── store.go
│   └── theme/
│       ├── go.mod               # module .../lib/theme — no dependencies
│       ├── theme.go
│       └── theme.css
├── apps/
│   └── app-name/
│       ├── go.mod               # module .../apps/app-name
│       ├── main.go              # entry point — load env, start server
│       ├── server.go            # embed, Server struct, routes, handlers
│       ├── db.go                # schema, model, CRUD functions
│       ├── auth.go              # session-validation middleware (if auth)
│       ├── templates/
│       │   ├── base.html        # layout with title/content blocks
│       │   └── *.html           # pages
│       ├── static/
│       │   └── styles.css
│       ├── Dockerfile
│       └── docker-compose.yml
├── .github/workflows/
│   ├── ci.yml                   # build/vet/test every module — no per-app list
│   └── docker.yml               # build + push images — per-app matrix
└── docker-compose.yml           # production compose — all apps
```

## The workspace (go.work)

The root `go.work` lists every module, joining them into one build for
in-workspace tooling. Cross-module imports resolve to local source — never to
a published version.

Note: `./...` from the bare workspace root does **not** span modules — the
root is not itself a module. To act on every module, iterate `go list -m`,
which enumerates the workspace:

```sh
for dir in $(go list -m -f '{{.Dir}}'); do
	(cd "$dir" && go build ./... && go vet ./... && go test ./...)
done
```

This is exactly what `ci.yml` runs, and it needs no per-app list.

```
go 1.25.0

use (
	./lib/auth
	./lib/store
	./lib/theme
	./apps/app-name
)
```

Commit `go.work` and `go.work.sum` — this is an app monorepo, not a library.

Each module's `go.mod` declares its path. The `lib/*` modules have **no
`require` block at all** — they are pure standard library. An app's `go.mod`
requires the libs at a placeholder version (`v0.0.0`) **and** carries a
`replace` directive pointing each at its local path:

```
module github.com/iammatthias/farfield/apps/app-name

go 1.25.0

require (
	github.com/iammatthias/farfield/lib/auth v0.0.0
	github.com/iammatthias/farfield/lib/cid v0.0.0
	github.com/iammatthias/farfield/lib/store v0.0.0
	github.com/iammatthias/farfield/lib/theme v0.0.0
	modernc.org/sqlite v1.50.1
)

// The lib/* modules are never published — resolve them from the local tree.
replace (
	github.com/iammatthias/farfield/lib/auth => ../../lib/auth
	github.com/iammatthias/farfield/lib/cid => ../../lib/cid
	github.com/iammatthias/farfield/lib/store => ../../lib/store
	github.com/iammatthias/farfield/lib/theme => ../../lib/theme
)
```

The `replace` block is **not optional**. `go.work` joins the modules for
in-workspace tooling, but a bare `v0.0.0` require still needs the `replace` to
resolve to source — without it, builds (per-module commands, the Docker build)
fail looking for a published `v0.0.0`. Every app `go.mod` carries both blocks.
Only the app pulls in `modernc.org/sqlite`; the libs stay dependency-free.

Do not add other modules. No web framework, no router, no ORM, no config
library, no logging library, no UUID library — see **What NOT to include**.

## Database Layer (db.go)

Pattern: one file per app, raw `database/sql`, standalone CRUD functions that
take `*sql.DB` as the first argument. No ORM.

The Rust andromeda stack used `Arc<Mutex<Connection>>` to serialize access.
The Go equivalent is **WAL mode + a busy timeout in the DSN** — concurrent
readers plus one writer, writers wait instead of erroring. `*sql.DB` is itself
a safe connection pool.

```go
package main

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

type Item struct {
	ID      int64  `json:"id"`
	ShortID string `json:"short_id"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

const schema = `
CREATE TABLE IF NOT EXISTS items (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	short_id TEXT NOT NULL UNIQUE,
	name     TEXT NOT NULL,
	content  TEXT NOT NULL
);`

// openDB opens the SQLite database, applies pragmas, and runs migrations.
func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)",
		path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// Session auth: also run db.Exec(store.SessionSchema) here.
	// Evolving schema: run column migrations + backfills here too —
	// see the self-migrating-sqlite skill.
	return db, nil
}

func createItem(db *sql.DB, name, content string) (*Item, error) {
	it := &Item{ShortID: store.ShortID(), Name: name, Content: content}
	res, err := db.Exec(
		`INSERT INTO items (short_id, name, content) VALUES (?, ?, ?)`,
		it.ShortID, it.Name, it.Content)
	if err != nil {
		return nil, err
	}
	it.ID, _ = res.LastInsertId()
	return it, nil
}

// getItem returns (nil, nil) when no row matches — not found is not an error.
func getItem(db *sql.DB, shortID string) (*Item, error) {
	var it Item
	err := db.QueryRow(
		`SELECT id, short_id, name, content FROM items WHERE short_id = ?`,
		shortID).Scan(&it.ID, &it.ShortID, &it.Name, &it.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func listItems(db *sql.DB) ([]Item, error) {
	rows, err := db.Query(
		`SELECT id, short_id, name, content FROM items ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.ShortID, &it.Name, &it.Content); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func deleteItem(db *sql.DB, shortID string) (bool, error) {
	res, err := db.Exec(`DELETE FROM items WHERE short_id = ?`, shortID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
```

### Key patterns

- **Model struct**: exported fields, `json:"..."` tags for the API.
- **Short IDs**: `store.ShortID()` — 10-char random ID (the `nanoid` equivalent).
- **Always parameterize**: `?` placeholders, never string concatenation.
- **`sql.ErrNoRows`**: map to `(nil, nil)` for get-by-id; it is not an error.
- **CRUD shape**: `createX` returns the created model; `getX` returns
  `(*Model, error)`; `listX` returns `([]Model, error)`; `deleteX` returns
  `(bool, error)`; `updateX` runs UPDATE then re-selects.
- If you want fully serialized writes instead of relying on WAL, add
  `db.SetMaxOpenConns(1)` — simplest correctness, slight throughput cost.

## Shared library: lib/store

`lib/store` holds the workspace-wide helpers — all standard library.

```go
package store

import (
	"bufio"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const idAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// ShortID returns a 10-character random ID (nanoid equivalent).
func ShortID() string {
	b := make([]byte, 10)
	rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}

// Env returns the environment variable, or def if unset or empty.
func Env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// LoadEnv finds the nearest .env — the working directory or an ancestor — and
// loads any KEY=VALUE pairs not already set. The whole monorepo shares one
// .env at the repo root, so an app started from anywhere resolves to it. A
// missing .env is not an error.
func LoadEnv() error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	for {
		path := filepath.Join(dir, ".env")
		if _, err := os.Stat(path); err == nil {
			return loadEnvFile(path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil // reached the filesystem root — no .env
		}
		dir = parent
	}
}

// loadEnvFile parses one .env file: blank lines and '#' comments are skipped,
// surrounding quotes are stripped, and keys already in the environment win.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// SessionSchema is the sessions table — apps using session auth run this in openDB.
const SessionSchema = `
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	expires_at INTEGER NOT NULL
);`

func InsertSession(db *sql.DB, token string, expiresAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO sessions (token, expires_at) VALUES (?, ?)`,
		token, expiresAt.Unix())
	return err
}

func ValidSession(db *sql.DB, token string) (bool, error) {
	var exp int64
	err := db.QueryRow(
		`SELECT expires_at FROM sessions WHERE token = ?`, token).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return time.Now().Unix() < exp, nil
}

func DeleteSession(db *sql.DB, token string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

func PruneSessions(db *sql.DB) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	return err
}
```

Go has no Cargo-style feature flags — the session helpers are simply exported.
Apps that don't need them don't call them.

## Shared library: lib/auth

`lib/auth` wraps the standard library primitives for password and cookie auth.
Zero dependencies.

```go
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

const cookieName = "session"

// constantTimeEqual compares a and b without leaking contents or length via
// timing — both sides are hashed first, so the buffers are always equal size.
func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// VerifyPassword reports whether input matches the expected password.
func VerifyPassword(input, expected string) bool {
	return constantTimeEqual(input, expected)
}

// VerifyAPIKey reports whether input matches the expected API key.
func VerifyAPIKey(input, expected string) bool {
	return constantTimeEqual(input, expected)
}

// NewSessionToken returns a cryptographically random token (Go 1.24+ rand.Text).
func NewSessionToken() string {
	return rand.Text()
}

// NewAPIKey returns a cryptographically random API key.
func NewAPIKey() string {
	return rand.Text()
}

// SessionCookie builds a session cookie. Pass secure=true for HTTPS deployments.
func SessionCookie(token string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 7, // 1 week
	}
}

// ClearCookie returns a cookie that expires the session immediately.
func ClearCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// Session reads the session token from the request cookie.
func Session(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}
```

## Server Layer (server.go)

### Embedded assets

Templates and static files are compiled into the binary with `embed`. Embed
paths are relative to the source file, so the binary is fully self-contained.

```go
import "embed"

//go:embed templates static
var assets embed.FS
```

Serve static files with `http.FileServerFS` (the mux pattern scopes it to
`/static/`, so nothing else in the FS is exposed):

```go
mux.Handle("GET /static/", http.FileServerFS(assets))
```

### App state

```go
type Server struct {
	db           *sql.DB
	templates    map[string]*template.Template
	password     string
	cookieSecure bool
}
```

Add domain-specific fields as needed.

### Templates

`html/template` with a `base.html` layout and one parsed template per page.
Each page is parsed together with the base, then rendered through it:

```go
import (
	"bytes"
	"html/template"
	"io/fs"
	"path"
)

func parseTemplates() (map[string]*template.Template, error) {
	pages, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	out := make(map[string]*template.Template)
	for _, page := range pages {
		name := path.Base(page)
		if name == "base.html" {
			continue
		}
		t, err := template.ParseFS(assets, "templates/base.html", page)
		if err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
}

// render writes a page through base.html. It buffers first, so a template
// error never produces a half-written response.
func (s *Server) render(w http.ResponseWriter, page string, data any) {
	t, ok := s.templates[page]
	if !ok {
		slog.Error("unknown template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		slog.Error("render failed", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}
```

### Routes

Two route sets — **web routes** (HTML pages + form posts) and **API routes**
(JSON) — plus embedded static assets. `http.ServeMux` does method-aware
routing and path wildcards natively since Go 1.22.

```go
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Web routes (HTML)
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /login", s.handlePostLogin)
	mux.HandleFunc("GET /logout", s.handleLogout)
	mux.HandleFunc("POST /items", s.requireAuth(s.handleCreateItem))
	mux.HandleFunc("GET /items/{id}", s.handleGetItem)

	// API routes (JSON)
	mux.HandleFunc("GET /api/items", s.handleAPIList)
	mux.HandleFunc("POST /api/items", s.handleAPICreate)
	mux.HandleFunc("GET /api/items/{id}", s.handleAPIGet)
	mux.HandleFunc("DELETE /api/items/{id}", s.handleAPIDelete)

	// Static assets (embedded)
	mux.Handle("GET /static/", http.FileServerFS(assets))

	return logRequests(mux)
}
```

Path values come from `r.PathValue("id")`.

### Middleware

Middleware is a plain `func(http.Handler) http.Handler`:

```go
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request",
			"method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
	})
}

// cors lets a browser on another origin (the website) read the public API
// and answers preflight. Wrap the mux in it for any app with a public API.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

`routes()` returns `cors(logRequests(mux))` for any app with a public API.

### Handlers

Web handlers read forms with `r.ParseForm()` / `r.FormValue`. API handlers
decode/encode JSON. A shared helper keeps API responses consistent:

```go
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleAPICreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	item, err := createItem(s.db, body.Name, body.Content)
	if err != nil {
		slog.Error("create item", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create item"})
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) handleGetItem(w http.ResponseWriter, r *http.Request) {
	item, err := getItem(s.db, r.PathValue("id"))
	if err != nil {
		slog.Error("get item", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if item == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "item.html", item)
}
```

### Flash messages via query params

Transient UI feedback rides through redirects on the query string — no server
session needed. Use `http.StatusSeeOther` (303) for post-redirect-get.

```go
http.Redirect(w, r, "/items/add?error="+url.QueryEscape("Name is required"),
	http.StatusSeeOther)
```

The receiving handler reads `r.URL.Query().Get("error")` and passes it to the
template, which renders it with `{{with .Error}}<p class="error">{{.}}</p>{{end}}`.

### Server startup with graceful shutdown

```go
func run(host, port string) error {
	db, err := openDB(store.Env("APP_DB_PATH", "app.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	tmpl, err := parseTemplates()
	if err != nil {
		return err
	}

	s := &Server{
		db:           db,
		templates:    tmpl,
		password:     store.Env("PASSWORD", ""),
		cookieSecure: store.Env("COOKIE_SECURE", "false") == "true",
	}

	srv := &http.Server{
		Addr:    net.JoinHostPort(host, port),
		Handler: s.routes(),
	}

	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
```

## Authentication

### Session/cookie auth (web-facing apps)

The standard pattern for apps with a login page. Auth state lives in the
`sessions` table (via `lib/store`), and a middleware guards protected routes.

`auth.go` in the app:

```go
package main

import (
	"net/http"

	"github.com/iammatthias/farfield/lib/auth"
	"github.com/iammatthias/farfield/lib/store"
)

// requireAuth wraps a handler, redirecting to /login when the session is invalid.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token, ok := auth.Session(r); ok {
			if valid, err := store.ValidSession(s.db, token); err == nil && valid {
				next(w, r)
				return
			}
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}
```

Login / logout handlers:

```go
func (s *Server) handlePostLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !auth.VerifyPassword(r.FormValue("password"), s.password) {
		http.Redirect(w, r, "/login?error="+url.QueryEscape("Invalid password"),
			http.StatusSeeOther)
		return
	}
	token := auth.NewSessionToken()
	if err := store.InsertSession(s.db, token, time.Now().Add(7*24*time.Hour)); err != nil {
		slog.Error("create session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, auth.SessionCookie(token, s.cookieSecure))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token, ok := auth.Session(r); ok {
		_ = store.DeleteSession(s.db, token)
	}
	http.SetCookie(w, auth.ClearCookie(s.cookieSecure))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
```

Protect a route by wrapping it: `s.requireAuth(s.handleCreateItem)`.
Remember `db.Exec(store.SessionSchema)` in `openDB` for session-auth apps.

### API-key auth (API-only apps)

For apps with no login page, use API-key middleware instead. It does not touch
`lib/auth` or the database — it is self-contained in `server.go`:

```go
func apiKeyAuth(key string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key == "" { // empty key disables auth
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

### Environment variables

| Variable       | Purpose                                  | Default      |
|----------------|------------------------------------------|--------------|
| `HOST`         | Bind address (`0.0.0.0` in Docker)       | `127.0.0.1`  |
| `PASSWORD`     | Shared password for every app's admin UI | none         |
| `COOKIE_SECURE`| `true` for HTTPS-only cookies            | `false`      |
| `APP_PORT`     | Listen port                              | `3000`       |
| `APP_DB_PATH`  | SQLite file path                         | `app.sqlite` |
| `APP_API_KEY`  | API key for the API-key auth pattern     | none (off)   |

The whole monorepo shares **one `.env` at the repo root** — `store.LoadEnv()`
walks up from the working directory to find it, so an app started from
anywhere resolves to the same file. Genuinely global values — `HOST`,
`PASSWORD`, `COOKIE_SECURE` — are unprefixed and shared by every app;
everything else is prefixed with the app name, *including the port*
(`BLOBS_PORT`, `CONTENT_PORT`, …), which `APP_*` above stands in for.

## Templates (html/template)

`templates/base.html` — layout with `block` slots. `block` provides both a
default and an overridable slot:

```html
{{define "base"}}<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>{{block "title" .}}Farfield{{end}}</title>
  <meta name="theme-color" content="#121113">
  <link rel="stylesheet" href="/static/styles.css?v={{.AssetVer}}">
</head>
<body>
  <main class="container">
    {{block "content" .}}{{end}}
  </main>
</body>
</html>{{end}}
```

`templates/index.html` — a page redefines the blocks:

```html
{{define "title"}}Items{{end}}
{{define "content"}}
  {{with .Error}}<p class="error">{{.}}</p>{{end}}
  {{range .Items}}
    <article><a href="/items/{{.ShortID}}">{{.Name}}</a></article>
  {{else}}
    <p>No items yet.</p>
  {{end}}
{{end}}
```

`html/template` escapes all interpolation by default — XSS-safe. For
pre-rendered trusted HTML, pass a `template.HTML` value (never user input).

## The shared stylesheet

`lib/theme` holds the workspace CSS (embedded as `theme.CSS`). Each app serves
it from a tiny handler, not from the per-app `static/` tree:

```go
func handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(w, theme.CSS)
}
// mux.HandleFunc("GET /static/styles.css", handleCSS)
```

A CDN caches that response, so a deploy of new CSS is invisible for up to an
hour. Fix it by **fingerprinting the URL** — hash the stylesheet once at
startup and stamp it into every page:

```go
// at startup:  s.assetVer = cid.Of([]byte(theme.CSS))[:16]
// in render(), before executing the template:
if m, ok := data.(map[string]any); ok {
	m["AssetVer"] = s.assetVer
}
```

base.html then references `/static/styles.css?v={{.AssetVer}}`. New build →
new hash → new URL → the CDN fetches fresh. `http.FileServerFS(assets)` still
serves any genuinely app-specific files under `/static/`.

## main.go

Minimal — load env, set up logging, start the server:

```go
package main

import (
	"log/slog"
	"os"

	"github.com/iammatthias/farfield/lib/store"
)

func main() {
	_ = store.LoadEnv() // finds the root .env, wherever the app is run from
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	host := store.Env("HOST", "127.0.0.1")
	port := store.Env("APP_PORT", "3000")

	if err := run(host, port); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
```

Keep `main.go` minimal — all logic lives in `server.go`, `db.go`, `auth.go`.

## Logging (log/slog)

Set the default handler once in `main()`. Then, throughout the app:

- `slog.Error("db query failed", "err", err)` — unrecoverable failures
- `slog.Warn("degraded", "err", err)` — non-critical issues
- `slog.Info("listening", "addr", addr)` — startup/lifecycle events

Always use key/value pairs, never `fmt.Sprintf` into the message.

## Dockerfile

Two stages. Because `modernc.org/sqlite` is pure Go, `CGO_ENABLED=0` yields a
static binary that runs on `distroless/static`. Built from the **repo root** so
`go.work` and the `lib/` modules are present:

```dockerfile
# Build from repo root: docker build -t APP_NAME -f apps/APP_NAME/Dockerfile .
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /bin/APP_NAME ./apps/APP_NAME

FROM gcr.io/distroless/static-debian12
COPY --from=build /bin/APP_NAME /APP_NAME
WORKDIR /data
EXPOSE 3000
ENV HOST=0.0.0.0 APP_PORT=3000
ENTRYPOINT ["/APP_NAME"]
```

Templates and static files are embedded in the binary — nothing else to copy.
`WORKDIR /data` is where the SQLite file is created (mount a volume there).

## docker-compose.yml (app-local)

Per-app compose for local dev — builds from source, context is the repo root:

```yaml
services:
  app:
    build:
      context: ../..
      dockerfile: apps/APP_NAME/Dockerfile
    ports:
      - "${APP_PORT:-3000}:3000"
    environment:
      - HOST=0.0.0.0
      - APP_PORT=3000
      - APP_DB_PATH=/data/APP_NAME.sqlite
      - PASSWORD=${PASSWORD:-changeme}
      - COOKIE_SECURE=false
    volumes:
      - app-data:/data
    restart: unless-stopped

volumes:
  app-data:
```

## The root `.env`

The whole monorepo shares one environment file: `.env` at the repo root.
`store.LoadEnv()` walks up from the working directory to find it, so every
app — run from anywhere — reads the same file. A committed `.env.example`
sits beside it as the template; `.env` itself is gitignored.

When you scaffold an app, append its variables (app-prefixed) to the root
`.env.example` — do not create a per-app env file:

```
# ── shared ──
HOST=127.0.0.1
COOKIE_SECURE=false
PASSWORD=changeme

# ── APP_NAME ──
APP_PORT=3000
APP_DB_PATH=app.sqlite
```

## Wiring up a new app

A `go.work` workspace makes this short. CI iterates `go list -m` to build and
test every module, so it needs **no per-app list** — registering the module in
`go.work` is enough. Only Docker image builds need a per-app entry.

### 1. Create the module and register it in `go.work`

```sh
mkdir -p apps/APP_NAME
cd apps/APP_NAME && go mod init github.com/iammatthias/farfield/apps/APP_NAME
cd ../.. && go work use ./apps/APP_NAME
```

`go work use` appends the module to `go.work`. Forgetting it means the app
builds only when your shell is inside its directory, and `./...` skips it.

### 2. Add dependencies to the app `go.mod`

Require the `lib/*` modules at `v0.0.0` with matching `replace` directives,
plus `modernc.org/sqlite`, then sync:

```sh
go work sync
```

Use `go work sync` — not per-module `go mod tidy` — to keep the workspace
consistent.

### 3. `.github/workflows/docker.yml` — add to the matrix

The Docker workflow builds one image per app. Add the app to the matrix list:

```yaml
strategy:
  matrix:
    app: [other-app, APP_NAME]
```

`ci.yml` needs no change — it discovers modules via `go list -m`.

### 4. Root `docker-compose.yml` — add a service + volume

The production compose at the repo root pulls images from the registry. Add a
service on a unique host port, plus a named volume if the app is stateful:

```yaml
services:
  APP_NAME:
    image: ghcr.io/iammatthias/farfield/APP_NAME:latest
    restart: unless-stopped
    ports:
      - "${APP_PORT:-3000}:3000"
    environment:
      - HOST=0.0.0.0
      - APP_PORT=3000
      - APP_DB_PATH=/data/APP_NAME.sqlite
      - PASSWORD=${PASSWORD:-changeme}
    volumes:
      - APP_NAME-data:/data

volumes:
  APP_NAME-data:
```

Drop `volumes`/`env_file` for a stateless app.

## Checklist

When scaffolding a new app:

1. `mkdir -p apps/APP_NAME && cd apps/APP_NAME && go mod init github.com/iammatthias/farfield/apps/APP_NAME`
2. `go work use ./apps/APP_NAME` from the repo root
3. Add `require` entries (`lib/*`, `modernc.org/sqlite`) **and matching `replace` directives** to the app `go.mod`
4. Write `db.go` — schema, model, CRUD (plus `store.SessionSchema` if auth)
5. Write `auth.go` — `requireAuth` middleware (if session auth is needed)
6. Write `server.go` — embed, `Server` struct, `run`, routes, `render`, handlers
7. Write `main.go` — minimal entry point
8. Create `templates/base.html` + page templates
9. Create `static/styles.css`
10. Append the app's variables (app-prefixed) to the root `.env.example`
11. Create `Dockerfile` + app-local `docker-compose.yml`
12. Add the app to the `matrix.app` list in `.github/workflows/docker.yml`
13. Add a service (+ volume) to the root `docker-compose.yml`
14. `go work sync`
15. Verify: `go run ./apps/APP_NAME`, hit the routes; then from `apps/APP_NAME`: `go build ./... && go vet ./... && go test ./...`
16. Verify: `docker build -f apps/APP_NAME/Dockerfile .` from the repo root

## Testing

Standard-library `testing` only. Co-locate `*_test.go` files with the code.
Use `t.TempDir()` for a throwaway SQLite file, `net/http/httptest` for handler
tests. To run every module's tests at once, iterate `go list -m` (as CI does).

## What NOT to include

The point of this stack is the standard library. Do not add:

- **No web framework** — `net/http` has method routing and path wildcards since Go 1.22 (no gin, echo, chi, fiber)
- **No router library** — `http.ServeMux` is enough
- **No ORM or query builder** — raw `database/sql` (no gorm, ent, sqlx, sqlc)
- **No cgo SQLite** — `modernc.org/sqlite` is pure Go; keeps `CGO_ENABLED=0` and static binaries (no mattn/go-sqlite3)
- **No template engine** — `html/template` (no templ, pongo2)
- **No config library** — env vars + `store.LoadEnv` (no viper, koanf)
- **No logging library** — `log/slog` (no zap, logrus, zerolog)
- **No UUID library** — `store.ShortID` / `crypto/rand` (no google/uuid)
- **No connection pool library** — `*sql.DB` + WAL + busy timeout

`modernc.org/sqlite` is the **only** external dependency. If a specific app
genuinely needs another, add it to that app's `go.mod` alone — never to a
`lib/*` module, and never to the workspace as a default.
