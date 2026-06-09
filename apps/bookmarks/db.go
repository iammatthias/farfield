package main

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Bookmark is one saved link. The ID is the stable key (a ShortID); the CID is
// a content hash that moves with every visible-field edit. AdminNotes is a
// private field that never leaves the admin UI — it is stripped from public
// API responses and excluded from the CID so its edits do not invalidate
// public ETag caches.
type Bookmark struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	Category      string `json:"category"`
	Public        bool   `json:"public"`
	AdminNotes    string `json:"adminNotes,omitempty"`
	OGTitle       string `json:"ogTitle,omitempty"`
	OGDescription string `json:"ogDescription,omitempty"`
	OGImage       string `json:"ogImage,omitempty"`
	OGSiteName    string `json:"ogSiteName,omitempty"`
	OGType        string `json:"ogType,omitempty"`
	MetaAuthor    string `json:"metaAuthor,omitempty"`
	Favicon       string `json:"favicon,omitempty"`
	CID           string `json:"cid"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

const schema = `
CREATE TABLE IF NOT EXISTS bookmarks (
	id              TEXT PRIMARY KEY,
	url             TEXT NOT NULL,
	title           TEXT NOT NULL DEFAULT '',
	description     TEXT NOT NULL DEFAULT '',
	category        TEXT NOT NULL DEFAULT '',
	public          INTEGER NOT NULL DEFAULT 0,
	admin_notes     TEXT NOT NULL DEFAULT '',
	og_title        TEXT NOT NULL DEFAULT '',
	og_description  TEXT NOT NULL DEFAULT '',
	og_image        TEXT NOT NULL DEFAULT '',
	og_site_name    TEXT NOT NULL DEFAULT '',
	og_type         TEXT NOT NULL DEFAULT '',
	meta_author     TEXT NOT NULL DEFAULT '',
	favicon         TEXT NOT NULL DEFAULT '',
	cid             TEXT NOT NULL DEFAULT '',
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS bookmarks_by_public_category
	ON bookmarks (public, category, created_at DESC);`

// bookmarkCols is the column list, in Bookmark-field order, shared by queries.
const bookmarkCols = `id, url, title, description, category, public, admin_notes, ` +
	`og_title, og_description, og_image, og_site_name, og_type, meta_author, ` +
	`favicon, cid, created_at, updated_at`

// openDB opens the SQLite database, applies pragmas, and migrates the schema.
// The migration sequence is idempotent — it brings any database, fresh or
// old, to the current schema on every startup. See the self-migrating-sqlite
// skill.
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
	// Columns added after the first release — CREATE TABLE IF NOT EXISTS will
	// not add these to a table that already exists.
	for _, c := range []struct{ col, decl string }{
		{"og_title", "TEXT NOT NULL DEFAULT ''"},
		{"og_description", "TEXT NOT NULL DEFAULT ''"},
		{"og_image", "TEXT NOT NULL DEFAULT ''"},
		{"og_site_name", "TEXT NOT NULL DEFAULT ''"},
		{"og_type", "TEXT NOT NULL DEFAULT ''"},
		{"meta_author", "TEXT NOT NULL DEFAULT ''"},
		{"favicon", "TEXT NOT NULL DEFAULT ''"},
		{"cid", "TEXT NOT NULL DEFAULT ''"},
		{"admin_notes", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := store.EnsureColumn(db, "bookmarks", c.col, c.decl); err != nil {
			return nil, err
		}
	}
	if err := backfillCIDs(db); err != nil {
		return nil, err
	}
	return db, nil
}

// bookmarkCID is the content identifier of a bookmark — a CIDv1 over its
// public-facing content. The id, admin notes, and timestamps are excluded so
// the CID tracks "what users see," and admin-only edits do not bump it.
func bookmarkCID(b *Bookmark) string {
	return cid.OfValue(map[string]any{
		"url":           b.URL,
		"title":         b.Title,
		"description":   b.Description,
		"category":      b.Category,
		"public":        b.Public,
		"ogTitle":       b.OGTitle,
		"ogDescription": b.OGDescription,
		"ogImage":       b.OGImage,
		"ogSiteName":    b.OGSiteName,
		"ogType":        b.OGType,
		"metaAuthor":    b.MetaAuthor,
		"favicon":       b.Favicon,
	})
}

// publicView returns a copy of the bookmark with admin-only fields cleared,
// safe to serialize to a public API response.
func publicView(b *Bookmark) *Bookmark {
	out := *b
	out.AdminNotes = ""
	return &out
}

// publicList returns a copy of bs with admin-only fields cleared from each
// element. The slice itself is fresh, so callers can mutate it freely.
func publicList(bs []Bookmark) []Bookmark {
	out := make([]Bookmark, len(bs))
	for i := range bs {
		out[i] = bs[i]
		out[i].AdminNotes = ""
	}
	return out
}

// backfillCIDs computes the content CID for any bookmark that lacks one — a
// one-time migration for databases created before CIDs.
func backfillCIDs(db *sql.DB) error {
	rows, err := db.Query(`SELECT ` + bookmarkCols + ` FROM bookmarks WHERE cid = ''`)
	if err != nil {
		return err
	}
	var bs []Bookmark
	for rows.Next() {
		b, err := scanBookmark(rows)
		if err != nil {
			rows.Close()
			return err
		}
		bs = append(bs, *b)
	}
	rows.Close()
	for i := range bs {
		if _, err := db.Exec(`UPDATE bookmarks SET cid = ? WHERE id = ?`,
			bookmarkCID(&bs[i]), bs[i].ID); err != nil {
			return err
		}
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(...any) error }

func scanBookmark(row scanner) (*Bookmark, error) {
	var b Bookmark
	var public int
	if err := row.Scan(&b.ID, &b.URL, &b.Title, &b.Description, &b.Category,
		&public, &b.AdminNotes, &b.OGTitle, &b.OGDescription, &b.OGImage,
		&b.OGSiteName, &b.OGType, &b.MetaAuthor, &b.Favicon, &b.CID,
		&b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, err
	}
	b.Public = public != 0
	return &b, nil
}

// listBookmarks returns every bookmark, newest first.
func listBookmarks(db *sql.DB) ([]Bookmark, error) {
	rows, err := db.Query(
		`SELECT ` + bookmarkCols + ` FROM bookmarks ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Bookmark{}
	for rows.Next() {
		b, err := scanBookmark(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// listPublicBookmarks returns every public bookmark, newest first.
func listPublicBookmarks(db *sql.DB) ([]Bookmark, error) {
	rows, err := db.Query(
		`SELECT ` + bookmarkCols + ` FROM bookmarks WHERE public = 1 ` +
			`ORDER BY category COLLATE NOCASE ASC, created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Bookmark{}
	for rows.Next() {
		b, err := scanBookmark(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// getBookmark returns a bookmark by id, or (nil, nil) if absent.
func getBookmark(db *sql.DB, id string) (*Bookmark, error) {
	b, err := scanBookmark(db.QueryRow(
		`SELECT `+bookmarkCols+` FROM bookmarks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// insertBookmark creates a bookmark, assigning a fresh id and timestamps.
func insertBookmark(db *sql.DB, b *Bookmark) error {
	if b.ID == "" {
		b.ID = store.ShortID()
	}
	b.URL = strings.TrimSpace(b.URL)
	b.CreatedAt = store.NowRFC3339()
	b.UpdatedAt = b.CreatedAt
	b.CID = bookmarkCID(b)
	pub := 0
	if b.Public {
		pub = 1
	}
	_, err := db.Exec(
		`INSERT INTO bookmarks (`+bookmarkCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.URL, b.Title, b.Description, b.Category, pub, b.AdminNotes,
		b.OGTitle, b.OGDescription, b.OGImage, b.OGSiteName, b.OGType,
		b.MetaAuthor, b.Favicon, b.CID, b.CreatedAt, b.UpdatedAt)
	return err
}

// updateBookmark replaces a bookmark in place, keyed by id, and stamps
// updated_at. The id, created_at, and existing CID are not read from b.
func updateBookmark(db *sql.DB, id string, b *Bookmark) (bool, error) {
	b.URL = strings.TrimSpace(b.URL)
	b.UpdatedAt = store.NowRFC3339()
	b.CID = bookmarkCID(b)
	pub := 0
	if b.Public {
		pub = 1
	}
	res, err := db.Exec(
		`UPDATE bookmarks SET url = ?, title = ?, description = ?, category = ?,
			public = ?, admin_notes = ?, og_title = ?, og_description = ?,
			og_image = ?, og_site_name = ?, og_type = ?, meta_author = ?,
			favicon = ?, cid = ?, updated_at = ? WHERE id = ?`,
		b.URL, b.Title, b.Description, b.Category, pub, b.AdminNotes,
		b.OGTitle, b.OGDescription, b.OGImage, b.OGSiteName, b.OGType,
		b.MetaAuthor, b.Favicon, b.CID, b.UpdatedAt, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	b.ID = id
	return true, nil
}

// deleteBookmark removes a bookmark by id.
func deleteBookmark(db *sql.DB, id string) (bool, error) {
	res, err := db.Exec(`DELETE FROM bookmarks WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CategoryGroup is one category's worth of bookmarks for the public index.
type CategoryGroup struct {
	Name      string
	Bookmarks []Bookmark
}

// groupByCategory partitions a sorted-by-category bookmark list into groups,
// preserving the input order within each group. An empty category sorts last
// under the label "Uncategorized."
func groupByCategory(bs []Bookmark) []CategoryGroup {
	const empty = "Uncategorized"
	order := []string{}
	idx := map[string]int{}
	for _, b := range bs {
		name := b.Category
		if strings.TrimSpace(name) == "" {
			name = empty
		}
		if _, ok := idx[name]; !ok {
			idx[name] = len(order)
			order = append(order, name)
		}
		i := idx[name]
		_ = i
	}
	// Move the empty bucket to the end if present.
	out := make([]CategoryGroup, 0, len(order))
	for _, name := range order {
		if name == empty {
			continue
		}
		out = append(out, CategoryGroup{Name: name})
	}
	hasEmpty := false
	for _, name := range order {
		if name == empty {
			hasEmpty = true
			break
		}
	}
	if hasEmpty {
		out = append(out, CategoryGroup{Name: empty})
	}
	// Refill in a single pass keyed by name.
	pos := map[string]int{}
	for i, g := range out {
		pos[g.Name] = i
	}
	for _, b := range bs {
		name := b.Category
		if strings.TrimSpace(name) == "" {
			name = empty
		}
		i := pos[name]
		out[i].Bookmarks = append(out[i].Bookmarks, b)
	}
	return out
}
