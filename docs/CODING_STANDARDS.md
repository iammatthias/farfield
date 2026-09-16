# Farfield Coding Standards

The rules every app in this workspace is held to. `README.md` covers the stack
and `BRAND.md` the visual language; this is about the code itself.

One idea underneath all of it: **the code is the documentation**. Anything a
reader can get from the code should come from the code, not from prose beside
it that can rot.

## Comments

A comment must carry something the code cannot. If a reader could derive it
from the line below, delete it.

Tautological comments are worse than no comments. They cost a line, they
decay silently when the code changes, and they train a reader to skim past
the comments that matter.

```go
// Bad — the signature already says this.
// openDB opens the database at path and returns it.
func openDB(path string) (*sql.DB, error)

// Bad — restates the statement.
limit := 50 // set limit to 50
```

What earns its place is the part the code cannot state: a constraint, a
consequence, a trap, a decision someone would otherwise undo.

```go
// Good — the number is arbitrary without it.
// maxCacheEntries bounds the memoized blob lookups. The map is keyed by
// content address and never invalidates, so without a bound it grows for the
// life of the process. Past the cap the whole map is dropped rather than
// evicted piecemeal — a CID's metadata is immutable, so nothing can be stale.
const maxCacheEntries = 4096

// Good — explains an ordering requirement that looks arbitrary.
// Lift recipe blocks out before goldmark sees the body, or it renders them
// as code and the parser never gets the bytes.
```

The test: delete the comment and ask whether anything was lost. If the answer
is no, it was never a comment — it was a restatement.

Exported identifiers still take doc comments where Go convention expects them,
but the same rule applies: say why it exists or what it guarantees, not what
its name already says.

## Types

Well-typed code needs less prose. When a comment exists to explain what a
value means, that is usually a missing type.

```go
// Bad — the comment is doing a type's job.
status string // "published", "draft", or "all"

// Good.
type entryStatus int
const (
	statusPublished entryStatus = iota
	statusDraft
	statusAll
)
```

Prefer a named type over a bare string or int whose legal values live in a
comment. Prefer a struct over a positional tuple of parameters that must be
passed in the right order.

## Tests

**Tests run real code.** A test exercises the real handler, the real database,
the real renderer. `openDB` against a temp file is a real database; a fake
that returns canned rows is not a test of anything we ship.

Do not mock our own code. Mocking a boundary we do not control — an upstream
HTTP service, a clock — is fine and sometimes necessary; standing up a stub
of our own function and asserting it was called proves only that the test
harness works.

**Tautological tests are harmful.** A test that asserts what it just set up
has negative value: it costs a run, it fails during honest refactors, and it
reports coverage of a path nothing verified.

```go
// Bad — asserts the literal it was just handed.
p := &Post{Slug: "abc"}
if p.Slug != "abc" { t.Fatal("slug wrong") }

// Good — asserts a behavior the code decides.
p := postFromForm(req)
if p.Slug == "" { t.Fatal("a post with no slug field got no generated slug") }
```

**Tests are hermetic.** A test's result must not depend on what else is
running on the machine. `apex`'s status test probed real localhost ports and
so passed on a clean laptop and failed whenever `make dev` was up — a test
that reports the developer's environment, not the code. If a test touches the
network or a port, it must pin the target somewhere nothing can answer.

Table-driven tests where the cases are genuinely parallel. A name per case, so
a failure says which one.

## Control flow

Flatten. An `if` chain that walks through five conditions to pick one of five
outcomes is a table.

```go
// Bad.
if kind == "blob" { ... } else if kind == "series" { ... } else if ... }

// Good.
var handlers = map[string]func(...){ "blob": ..., "series": ... }
```

Return early. Handle the error and get out, so the happy path stays at one
indent level and reads top to bottom.

Prefer a function that does one thing over a parameter that switches what a
function does. A boolean argument at a call site is unreadable — `save(p,
true)` says nothing — so if it must exist, make it a named type.

## Duplication

Used once, keep it local. Used twice, look hard. Used three times, it belongs
in `lib/`.

`lib/` is the shared floor: `web` for HTTP shape (auth, middleware, rendering,
JSON), `store` for database ceremony, `markdown` for body rendering, `cid` for
content addressing. An app should reach for what is there before growing its
own. A helper that three apps wrote separately is a bug in `lib/`, not an
accident.

## File size

A file holds one subject. When it outgrows the subject, split it by
responsibility rather than by line count — `server.go` holding routing and
handlers and rendering and the API is four files that share a name.

Anything over roughly 500 lines deserves the question. Over 1,000 is a defect
to schedule, not a style preference. The same applies to functions: a function
longer than a screen is usually several functions that have not been named.

## Errors

An error message is read by someone who cannot see the code. Say what failed
and what was being attempted, in lowercase, without punctuation at the end.
Wrap with `%w` when the caller might reasonably inspect the cause.

Never discard an error to make a signature tidy. If it truly cannot happen,
the reason is a comment that earns its place.
