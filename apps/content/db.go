package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// errSlugTaken is returned when a collection or entry slug is already in use.
var errSlugTaken = errors.New("that slug is already taken")

// Collection is a named group of entries — created and managed from the admin
// UI. Its slug is its stable key.
type Collection struct {
	ID          int64  `json:"-"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"createdAt"`
	EntryCount  int    `json:"entryCount"` // filled by list views
}

// Entry is one piece of long-form content. Its body is markdown; images are
// referenced inline as blob://<cid> and resolved against the blobs service.
// Slug is the stable key; CID is the content hash — it changes whenever the
// content does, giving inherent versioning and verifiability.
type Entry struct {
	ID         int64    `json:"-"`
	Collection string   `json:"collection"` // collection slug
	Slug       string   `json:"slug"`
	CID        string   `json:"cid"`
	Title      string   `json:"title"`
	Excerpt    string   `json:"excerpt,omitempty"`
	Body       string   `json:"body"`
	Tags       []string `json:"tags"`
	Published  bool     `json:"published"`
	CreatedAt  string   `json:"createdAt"`
	UpdatedAt  string   `json:"updatedAt"`
}

// Series is a reusable markdown fragment — typically a gallery. An entry body
// embeds one with series://<slug>; the website splices the fragment's rendered
// markdown into the parent post in its place.
type Series struct {
	Slug      string `json:"slug"`
	CID       string `json:"cid"`
	Title     string `json:"title,omitempty"`
	Body      string `json:"body"` // markdown
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

const schema = `
CREATE TABLE IF NOT EXISTS collections (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	slug        TEXT NOT NULL UNIQUE,
	name        TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS entries (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
	slug          TEXT NOT NULL UNIQUE,
	title         TEXT NOT NULL,
	excerpt       TEXT NOT NULL DEFAULT '',
	body          TEXT NOT NULL DEFAULT '',
	tags          TEXT NOT NULL DEFAULT '[]',
	published     INTEGER NOT NULL DEFAULT 0,
	created_at    TEXT NOT NULL,
	updated_at    TEXT NOT NULL,
	cid           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS entries_by_collection ON entries (collection_id, created_at DESC);
CREATE INDEX IF NOT EXISTS entries_by_created ON entries (created_at DESC);

CREATE TABLE IF NOT EXISTS series (
	slug       TEXT PRIMARY KEY,
	title      TEXT NOT NULL DEFAULT '',
	body       TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	cid        TEXT NOT NULL DEFAULT ''
);`

// openDB opens the SQLite database, applies pragmas, and migrates. Foreign
// keys are on so deleting a collection cascades to its entries.
func openDB(path string) (*sql.DB, error) {
	db, err := store.OpenDB(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if _, err := db.Exec(store.SessionSchema); err != nil {
		return nil, err
	}
	// Migrate pre-rename databases: the series stable key went rkey -> slug.
	if err := store.RenameColumn(db, "series", "rkey", "slug"); err != nil {
		return nil, err
	}
	// Migrate databases created before CIDs: add the column, then backfill.
	if err := store.EnsureColumn(db, "entries", "cid", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := store.EnsureColumn(db, "series", "cid", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := backfillCIDs(db); err != nil {
		return nil, err
	}
	return db, nil
}

func encodeTags(tags []string) string {
	if tags == nil {
		tags = []string{}
	}
	b, _ := json.Marshal(tags)
	return string(b)
}

func decodeTags(s string) []string {
	out := []string{}
	if s != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// entryCID is the content identifier of an entry: a CIDv1 over its canonical
// content — collection, title, excerpt, body, tags, published. The slug (the
// key) and timestamps are excluded, so the CID tracks content, not metadata.
func entryCID(e *Entry) string {
	tags := e.Tags
	if tags == nil {
		tags = []string{}
	}
	return cid.OfValue(map[string]any{
		"collection": e.Collection,
		"title":      e.Title,
		"excerpt":    e.Excerpt,
		"body":       e.Body,
		"tags":       tags,
		"published":  e.Published,
	})
}

// seriesCID is the content identifier of a series fragment.
func seriesCID(s *Series) string {
	return cid.OfValue(map[string]any{
		"title": s.Title,
		"body":  s.Body,
	})
}

// backfillCIDs computes the content CID for any entry or series row that
// lacks one — a one-time migration for databases created before CIDs.
func backfillCIDs(db *sql.DB) error {
	rows, err := db.Query(`SELECT e.slug, c.slug, e.title, e.excerpt, e.body, e.tags, e.published
		FROM entries e JOIN collections c ON c.id = e.collection_id WHERE e.cid = ''`)
	if err != nil {
		return err
	}
	type tagged struct {
		slug string
		e    Entry
	}
	var entries []tagged
	for rows.Next() {
		var slug, tags string
		var e Entry
		if err := rows.Scan(&slug, &e.Collection, &e.Title, &e.Excerpt,
			&e.Body, &tags, &e.Published); err != nil {
			rows.Close()
			return err
		}
		e.Tags = decodeTags(tags)
		entries = append(entries, tagged{slug, e})
	}
	rows.Close()
	for _, t := range entries {
		if _, err := db.Exec(`UPDATE entries SET cid = ? WHERE slug = ?`,
			entryCID(&t.e), t.slug); err != nil {
			return err
		}
	}

	srows, err := db.Query(`SELECT slug, title, body FROM series WHERE cid = ''`)
	if err != nil {
		return err
	}
	var seriesRows []Series
	for srows.Next() {
		var s Series
		if err := srows.Scan(&s.Slug, &s.Title, &s.Body); err != nil {
			srows.Close()
			return err
		}
		seriesRows = append(seriesRows, s)
	}
	srows.Close()
	for i := range seriesRows {
		if _, err := db.Exec(`UPDATE series SET cid = ? WHERE slug = ?`,
			seriesCID(&seriesRows[i]), seriesRows[i].Slug); err != nil {
			return err
		}
	}
	return nil
}

// ── collections ────────────────────────────────────────────────────────────

func scanCollection(row interface{ Scan(...any) error }) (*Collection, error) {
	var c Collection
	if err := row.Scan(&c.ID, &c.Slug, &c.Name, &c.Description, &c.CreatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

// listCollections returns every collection with its entry count, by name.
func listCollections(db *sql.DB) ([]Collection, error) {
	rows, err := db.Query(`
		SELECT c.id, c.slug, c.name, c.description, c.created_at,
		       (SELECT COUNT(*) FROM entries e WHERE e.collection_id = c.id)
		FROM collections c ORDER BY c.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Collection
	for rows.Next() {
		var c Collection
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.Description,
			&c.CreatedAt, &c.EntryCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// countCollections returns the number of collections — a cheap status probe
// that avoids loading every row.
func countCollections(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM collections`).Scan(&n)
	return n, err
}

// getCollection returns a collection by slug, or (nil, nil) if absent.
func getCollection(db *sql.DB, slug string) (*Collection, error) {
	c, err := scanCollection(db.QueryRow(
		`SELECT id, slug, name, description, created_at FROM collections WHERE slug = ?`,
		slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// insertCollection creates a collection. The slug must be unique.
func insertCollection(db *sql.DB, c *Collection) error {
	c.CreatedAt = store.NowRFC3339()
	res, err := db.Exec(
		`INSERT INTO collections (slug, name, description, created_at) VALUES (?, ?, ?, ?)`,
		c.Slug, c.Name, c.Description, c.CreatedAt)
	if err != nil {
		if isUnique(err) {
			return errSlugTaken
		}
		return err
	}
	c.ID, _ = res.LastInsertId()
	return nil
}

// updateCollection updates a collection's name and description (the slug is
// immutable — it is the stable key).
func updateCollection(db *sql.DB, slug, name, description string) (bool, error) {
	res, err := db.Exec(
		`UPDATE collections SET name = ?, description = ? WHERE slug = ?`,
		name, description, slug)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// deleteCollection removes a collection and (by cascade) all of its entries.
func deleteCollection(db *sql.DB, slug string) (bool, error) {
	res, err := db.Exec(`DELETE FROM collections WHERE slug = ?`, slug)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ── entries ────────────────────────────────────────────────────────────────

const entryCols = `e.id, c.slug, e.slug, e.cid, e.title, e.excerpt, e.body, e.tags,
	e.published, e.created_at, e.updated_at`

// entryListCols is the body-less projection for list views: the same shape as
// entryCols — so scanEntry works unchanged — with ” standing in for the
// (potentially large) markdown body.
const entryListCols = `e.id, c.slug, e.slug, e.cid, e.title, e.excerpt, '' AS body, e.tags,
	e.published, e.created_at, e.updated_at`

func scanEntry(row interface{ Scan(...any) error }) (*Entry, error) {
	var e Entry
	var tags string
	if err := row.Scan(&e.ID, &e.Collection, &e.Slug, &e.CID, &e.Title, &e.Excerpt,
		&e.Body, &tags, &e.Published, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	e.Tags = decodeTags(tags)
	return &e, nil
}

// listEntries returns entries without bodies (Body == ""), newest first — the
// projection for list views, which never render bodies. collection filters by
// collection slug ("" = all); publishedOnly restricts to published entries;
// limit caps the result (<= 0 means no limit).
func listEntries(db *sql.DB, collection string, publishedOnly bool, limit int) ([]Entry, error) {
	return queryEntries(db, entryListCols, collection, publishedOnly, limit, 0)
}

// listEntriesFull is listEntries with full markdown bodies, plus an offset for
// paging — for the public API and exports, whose consumers render bodies.
func listEntriesFull(db *sql.DB, collection string, publishedOnly bool, limit, offset int) ([]Entry, error) {
	return queryEntries(db, entryCols, collection, publishedOnly, limit, offset)
}

func queryEntries(db *sql.DB, cols, collection string, publishedOnly bool, limit, offset int) ([]Entry, error) {
	q := `SELECT ` + cols + `
	      FROM entries e JOIN collections c ON c.id = e.collection_id`
	var where []string
	var args []any
	if collection != "" {
		where = append(where, "c.slug = ?")
		args = append(args, collection)
	}
	if publishedOnly {
		where = append(where, "e.published = 1")
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY e.created_at DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
		if offset > 0 {
			q += " OFFSET ?"
			args = append(args, offset)
		}
	}

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// countEntries returns the number of entries, optionally filtered to one
// collection and to published entries only.
func countEntries(db *sql.DB, collection string, publishedOnly bool) (int, error) {
	q := `SELECT COUNT(*) FROM entries e`
	var where []string
	var args []any
	if collection != "" {
		q += ` JOIN collections c ON c.id = e.collection_id`
		where = append(where, "c.slug = ?")
		args = append(args, collection)
	}
	if publishedOnly {
		where = append(where, "e.published = 1")
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	var n int
	err := db.QueryRow(q, args...).Scan(&n)
	return n, err
}

// entriesFingerprint returns a cheap change fingerprint for the published
// entry list — "<count>-<max updated_at>" — used to derive a list-level ETag
// without loading any bodies. collection filters by collection slug ("" = all).
func entriesFingerprint(db *sql.DB, collection string) (string, error) {
	q := `SELECT COUNT(*) || '-' || COALESCE(MAX(e.updated_at), '') FROM entries e`
	var args []any
	if collection != "" {
		q += ` JOIN collections c ON c.id = e.collection_id WHERE e.published = 1 AND c.slug = ?`
		args = append(args, collection)
	} else {
		q += ` WHERE e.published = 1`
	}
	var fp string
	err := db.QueryRow(q, args...).Scan(&fp)
	return fp, err
}

// seriesFingerprint is entriesFingerprint for the series table.
func seriesFingerprint(db *sql.DB) (string, error) {
	var fp string
	err := db.QueryRow(
		`SELECT COUNT(*) || '-' || COALESCE(MAX(updated_at), '') FROM series`).Scan(&fp)
	return fp, err
}

// getEntry returns an entry by slug, or (nil, nil) if absent.
func getEntry(db *sql.DB, slug string) (*Entry, error) {
	e, err := scanEntry(db.QueryRow(
		`SELECT `+entryCols+`
		 FROM entries e JOIN collections c ON c.id = e.collection_id
		 WHERE e.slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// insertEntry creates an entry in the named collection. The slug is stamped
// with the creation instant — "<unixMillis>-<slug>" — so app-authored entries
// are keyed like the migrated content; it must then be unique. An unknown
// collection slug is an error.
func insertEntry(db *sql.DB, e *Entry) error {
	collID, err := collectionID(db, e.Collection)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	e.CreatedAt = now.Format(time.RFC3339)
	e.UpdatedAt = e.CreatedAt
	e.Slug = stampSlug(e.Slug, now)
	e.CID = entryCID(e)
	res, err := db.Exec(
		`INSERT INTO entries
		   (collection_id, slug, title, excerpt, body, tags, published, created_at, updated_at, cid)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		collID, e.Slug, e.Title, e.Excerpt, e.Body, encodeTags(e.Tags),
		e.Published, e.CreatedAt, e.UpdatedAt, e.CID)
	if err != nil {
		if isUnique(err) {
			return errSlugTaken
		}
		return err
	}
	e.ID, _ = res.LastInsertId()
	return nil
}

// updateEntry replaces an entry, identified by its current slug. e carries the
// new values (including a possibly-changed collection and slug).
func updateEntry(db *sql.DB, currentSlug string, e *Entry) error {
	collID, err := collectionID(db, e.Collection)
	if err != nil {
		return err
	}
	e.UpdatedAt = store.NowRFC3339()
	e.CID = entryCID(e)
	res, err := db.Exec(
		`UPDATE entries SET collection_id = ?, slug = ?, title = ?, excerpt = ?,
		   body = ?, tags = ?, published = ?, updated_at = ?, cid = ?
		 WHERE slug = ?`,
		collID, e.Slug, e.Title, e.Excerpt, e.Body, encodeTags(e.Tags),
		e.Published, e.UpdatedAt, e.CID, currentSlug)
	if err != nil {
		if isUnique(err) {
			return errSlugTaken
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// deleteEntry removes an entry by slug.
func deleteEntry(db *sql.DB, slug string) (bool, error) {
	res, err := db.Exec(`DELETE FROM entries WHERE slug = ?`, slug)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// getOrCreateCollection returns the collection with the given slug, creating
// it with the given name if it does not yet exist.
func getOrCreateCollection(db *sql.DB, slug, name string) (*Collection, error) {
	c, err := getCollection(db, slug)
	if err != nil {
		return nil, err
	}
	if c != nil {
		return c, nil
	}
	c = &Collection{Slug: slug, Name: name}
	if err := insertCollection(db, c); err != nil {
		return nil, err
	}
	return c, nil
}

// importEntry inserts or replaces an entry, keyed by slug, preserving the
// created/updated timestamps it carries — unlike insertEntry, which stamps
// them with the current time.
func importEntry(db *sql.DB, e *Entry) error {
	collID, err := collectionID(db, e.Collection)
	if err != nil {
		return err
	}
	if e.CreatedAt == "" {
		e.CreatedAt = store.NowRFC3339()
	}
	if e.UpdatedAt == "" {
		e.UpdatedAt = e.CreatedAt
	}
	e.CID = entryCID(e)
	_, err = db.Exec(
		`INSERT INTO entries
		   (collection_id, slug, title, excerpt, body, tags, published, created_at, updated_at, cid)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(slug) DO UPDATE SET
		   collection_id=excluded.collection_id, title=excluded.title,
		   excerpt=excluded.excerpt, body=excluded.body, tags=excluded.tags,
		   published=excluded.published, created_at=excluded.created_at,
		   updated_at=excluded.updated_at, cid=excluded.cid`,
		collID, e.Slug, e.Title, e.Excerpt, e.Body, encodeTags(e.Tags),
		e.Published, e.CreatedAt, e.UpdatedAt, e.CID)
	return err
}

// collectionID resolves a collection slug to its row id.
func collectionID(db *sql.DB, slug string) (int64, error) {
	var id int64
	err := db.QueryRow(`SELECT id FROM collections WHERE slug = ?`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("unknown collection %q", slug)
	}
	return id, err
}

// ── series ─────────────────────────────────────────────────────────────────

const seriesCols = `slug, cid, title, body, created_at, updated_at`

func scanSeries(row interface{ Scan(...any) error }) (*Series, error) {
	var s Series
	if err := row.Scan(&s.Slug, &s.CID, &s.Title, &s.Body,
		&s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

// listSeries returns every series fragment, newest first.
func listSeries(db *sql.DB) ([]Series, error) {
	rows, err := db.Query(`SELECT ` + seriesCols + ` FROM series ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Series
	for rows.Next() {
		s, err := scanSeries(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// getSeries returns a series by slug, or (nil, nil) if absent.
func getSeries(db *sql.DB, slug string) (*Series, error) {
	s, err := scanSeries(db.QueryRow(
		`SELECT `+seriesCols+` FROM series WHERE slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// upsertSeries inserts or replaces a series fragment, keyed by slug. The
// caller sets the timestamps (now for admin edits, the original for imports).
func upsertSeries(db *sql.DB, s *Series) error {
	s.CID = seriesCID(s)
	_, err := db.Exec(
		`INSERT INTO series (slug, title, body, created_at, updated_at, cid)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(slug) DO UPDATE SET
		   title=excluded.title, body=excluded.body,
		   updated_at=excluded.updated_at, cid=excluded.cid`,
		s.Slug, s.Title, s.Body, s.CreatedAt, s.UpdatedAt, s.CID)
	return err
}

// deleteSeries removes a series fragment by slug.
func deleteSeries(db *sql.DB, slug string) (bool, error) {
	res, err := db.Exec(`DELETE FROM series WHERE slug = ?`, slug)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// isUnique reports whether err is a SQLite UNIQUE-constraint violation.
func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
