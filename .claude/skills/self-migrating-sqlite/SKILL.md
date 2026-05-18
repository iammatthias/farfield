---
name: Self-Migrating SQLite
description: Make a Go service's embedded SQLite database migrate its own schema on startup. openDB runs CREATE TABLE IF NOT EXISTS plus idempotent ensureColumn / renameColumn / backfill helpers, so shipping new code brings every old database current with zero migration tooling and zero ops steps. Use for any single-binary Go app with a SQLite database whose schema will evolve.
---

# Self-Migrating SQLite

## Overview

A single-binary service should not need a migration tool, a migrations
directory, or a manual "run migrations" step. The database migrates **itself**:
`openDB` brings any database — fresh or years old — to the current schema, and
every step is safe to run on every startup. Deploy new code; the next process
that opens the file does the migration. That is the entire ops procedure.

This works because SQLite schema changes are cheap and inspectable, and the set
of operations a forward-only app needs is small: create tables, add columns,
rename columns, backfill values.

## The openDB discipline

`openDB` runs a fixed, ordered sequence. Each step is idempotent.

```go
func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}

	// 1. Current schema. CREATE TABLE IF NOT EXISTS: builds fresh DBs,
	//    no-ops on existing ones. (It never alters an existing table.)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}

	// 2. Renames — before anything reads the new column name.
	if err := renameColumn(db, "posts", "id", "slug"); err != nil {
		return nil, err
	}

	// 3. Added columns — CREATE TABLE IF NOT EXISTS won't add these
	//    to a table that already exists.
	if err := ensureColumn(db, "posts", "cid", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}

	// 4. Backfill — populate new columns for pre-existing rows.
	if err := backfillCIDs(db); err != nil {
		return nil, err
	}
	return db, nil
}
```

**Order matters:** renames before added-columns before backfills before any
code that reads the new shape. A backfill that selects `slug` must run after
the `id -> slug` rename.

## The DSN

```
file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)
```

WAL gives concurrent readers plus one writer; `busy_timeout` makes writers wait
instead of erroring under contention. This matters during migration: a one-off
migration process (`docker compose run`) and the live server can hold the same
file open without `database is locked` failures.

Use `modernc.org/sqlite` — pure Go, no cgo, so the binary stays static
(`CGO_ENABLED=0`).

## ensureColumn

`CREATE TABLE IF NOT EXISTS` does **not** add a column to a table that already
exists. A new column needs an explicit `ALTER`, guarded so it runs once:

```go
// ensureColumn adds a column to a table if it is not already present.
// The table/column/decl are code constants, so the built DDL is safe.
func ensureColumn(db *sql.DB, table, column, decl string) error {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil // already there — nothing to do
	}
	_, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + decl)
	return err
}
```

Give the new column a `DEFAULT` so existing rows are valid immediately
(`TEXT NOT NULL DEFAULT ''`); a backfill then replaces the placeholder.

## renameColumn

Rename only when the old name still exists and the new one does not — so it
fires exactly once across the database's life:

```go
// renameColumn renames a table column when the old name still exists and
// the new one does not — a one-time migration for pre-rename databases.
func renameColumn(db *sql.DB, table, oldName, newName string) error {
	var hasOld, hasNew int
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?),
		(SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?)`,
		table, oldName, table, newName).Scan(&hasOld, &hasNew); err != nil {
		return err
	}
	if hasOld == 0 || hasNew > 0 {
		return nil
	}
	_, err := db.Exec("ALTER TABLE " + table + " RENAME COLUMN " + oldName + " TO " + newName)
	return err
}
```

Update the `schema` constant to the new name at the same time, so fresh
databases are born correct and `renameColumn` is a no-op for them.
`ALTER TABLE … RENAME COLUMN` needs SQLite 3.25+ (modernc is current).

## Backfill

A backfill computes values for rows that predate a column. Make it
self-limiting — select only the rows still needing work — so it is free on
every subsequent startup:

```go
func backfillCIDs(db *sql.DB) error {
	rows, err := db.Query(`SELECT slug, body FROM posts WHERE cid = ''`)
	// ... read rows, close, then UPDATE each with its computed value
}
```

Once every row has a value, the `WHERE cid = ''` selects nothing and the
backfill is a single cheap query.

## Idempotency is the whole contract

Every step must be a no-op when already applied — that is what lets the whole
sequence run unconditionally on every open. The guards (`COUNT(*) FROM
pragma_table_info`, `WHERE cid = ''`, `IF NOT EXISTS`) are not optimizations;
they are the correctness condition.

## Gotchas

- `CREATE TABLE IF NOT EXISTS` never alters an existing table — added columns
  need `ensureColumn`, not a tweak to the schema constant.
- The DDL strings are built by concatenation. That is safe **only** because
  table/column names are compile-time constants. Never interpolate user input
  into DDL.
- A `RENAME COLUMN` on a `PRIMARY KEY` column works — SQLite updates the
  constraint with it.
- Run a one-off migration with `docker compose run --rm <svc> <subcommand>`,
  or just let the next normal startup do it. WAL + `busy_timeout` make a
  concurrent open safe; for a destructive one-off, stopping the service first
  is still cleaner.

## Anti-patterns

- **A separate migration tool or `migrations/` directory** for a single-binary
  service. The binary already opens the database; let it migrate.
- **Numbered migration files** with a `schema_version` table. For a
  forward-only app, presence checks (`pragma_table_info`) are simpler and need
  no version bookkeeping.
- **DROP-and-recreate to "migrate."** That is data loss. Add and rename in
  place; SQLite's `ALTER TABLE` covers the forward-only cases.
- **Skipping the guard** because "it'll only run once." It runs on every
  startup. Unguarded `ALTER` fails the second time and crashes the service.
