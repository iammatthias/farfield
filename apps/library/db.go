package main

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Book is one EPUB in the catalog. The CID is the content identifier of the
// EPUB bytes; CoverCID, when set, is the CID of the extracted cover image.
// Both are keys into the ByteStore, where the bytes themselves live.
type Book struct {
	CID         string `json:"cid"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Language    string `json:"language"`
	Identifier  string `json:"identifier"`
	Description string `json:"description"`
	Collection  string `json:"collection"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	CoverCID    string `json:"coverCid"`
	CoverMime   string `json:"coverMime"`
	CreatedAt   string `json:"createdAt"`
}

// schema is the book metadata index. The created_at index backs both the admin
// UI listing and the OPDS feed, newest first.
const schema = `
CREATE TABLE IF NOT EXISTS books (
	cid         TEXT PRIMARY KEY,
	title       TEXT NOT NULL DEFAULT '',
	author      TEXT NOT NULL DEFAULT '',
	language    TEXT NOT NULL DEFAULT '',
	identifier  TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	collection  TEXT NOT NULL DEFAULT '',
	filename    TEXT NOT NULL DEFAULT '',
	size        INTEGER NOT NULL,
	cover_cid   TEXT NOT NULL DEFAULT '',
	cover_mime  TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS books_by_created ON books (created_at DESC, cid);`

// bookCols is the column list, in Book-field order, shared by every query.
const bookCols = `cid, title, author, language, identifier, description, collection, filename, size, cover_cid, cover_mime, created_at`

// openDB opens the SQLite database, applies pragmas, and migrates. It holds the
// book metadata index and admin login sessions; book and cover bytes live in
// the ByteStore.
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
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if _, err := db.Exec(store.SessionSchema); err != nil {
		return nil, err
	}
	// Self-migrate: add the collection column to databases created before it
	// existed, then index it (the index can only be built once the column is).
	if err := ensureColumn(db, "books", "collection", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if _, err := db.Exec(
		`CREATE INDEX IF NOT EXISTS books_by_collection ON books (collection, created_at DESC)`); err != nil {
		return nil, err
	}
	return db, nil
}

// ensureColumn adds a column to a table when it is missing, so an existing
// database picks up a new field on deploy with no migration tooling.
func ensureColumn(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl)
	return err
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(...any) error }

func scanBook(row scanner) (*Book, error) {
	var b Book
	err := row.Scan(&b.CID, &b.Title, &b.Author, &b.Language, &b.Identifier,
		&b.Description, &b.Collection, &b.Filename, &b.Size, &b.CoverCID, &b.CoverMime, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// upsertBook inserts a book's metadata, replacing any existing row for the same
// CID (re-uploading identical bytes is idempotent).
func upsertBook(db *sql.DB, b *Book) error {
	_, err := db.Exec(
		`INSERT INTO books (`+bookCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(cid) DO UPDATE SET
		   title=excluded.title, author=excluded.author, language=excluded.language,
		   identifier=excluded.identifier, description=excluded.description,
		   collection=excluded.collection, filename=excluded.filename, size=excluded.size,
		   cover_cid=excluded.cover_cid, cover_mime=excluded.cover_mime,
		   created_at=excluded.created_at`,
		b.CID, b.Title, b.Author, b.Language, b.Identifier, b.Description,
		b.Collection, b.Filename, b.Size, b.CoverCID, b.CoverMime, b.CreatedAt)
	return err
}

// getBook returns a book's metadata, or (nil, nil) when no such CID exists.
func getBook(db *sql.DB, cid string) (*Book, error) {
	b, err := scanBook(db.QueryRow(
		`SELECT `+bookCols+` FROM books WHERE cid = ?`, cid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// listBooks returns book metadata, newest first. A negative limit returns every
// book (used by the OPDS feed); a positive limit paginates the admin UI.
func listBooks(db *sql.DB, limit, offset int) ([]Book, error) {
	rows, err := db.Query(
		`SELECT `+bookCols+` FROM books ORDER BY created_at DESC, cid LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// countBooks returns the total number of books in the index.
func countBooks(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&n)
	return n, err
}

// listBooksByCollection returns every book in a collection, newest first. An
// empty collection name selects the uncategorised books.
func listBooksByCollection(db *sql.DB, collection string) ([]Book, error) {
	rows, err := db.Query(
		`SELECT `+bookCols+` FROM books WHERE collection = ? ORDER BY created_at DESC, cid`,
		collection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// CollectionStat is one named collection (folder) and how many books it holds.
type CollectionStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// collectionStats returns the named collections with their book counts (sorted
// by name) and, separately, the number of uncategorised books.
func collectionStats(db *sql.DB) (named []CollectionStat, uncategorized int, err error) {
	rows, err := db.Query(
		`SELECT collection, COUNT(*) FROM books GROUP BY collection ORDER BY collection`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, 0, err
		}
		if name == "" {
			uncategorized = n
			continue
		}
		named = append(named, CollectionStat{Name: name, Count: n})
	}
	return named, uncategorized, rows.Err()
}

// updateBookCollection moves a book into a collection. It reports whether a
// book with that CID existed.
func updateBookCollection(db *sql.DB, cid, collection string) (bool, error) {
	res, err := db.Exec(`UPDATE books SET collection = ? WHERE cid = ?`, collection, cid)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// coverInfo reports whether any book references coverCID as its cover, and the
// cover's MIME type. It backs both the cover endpoint (a cover is only served
// when it belongs to a book) and delete cleanup (a cover's bytes are only
// removed once no book references them).
func coverInfo(db *sql.DB, coverCID string) (mime string, exists bool, err error) {
	err = db.QueryRow(
		`SELECT cover_mime FROM books WHERE cover_cid = ? LIMIT 1`, coverCID).Scan(&mime)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return mime, true, nil
}

// deleteBook removes a book's metadata and returns the deleted row (nil when no
// such CID existed) so the caller can clean up the EPUB and cover bytes.
func deleteBook(db *sql.DB, cid string) (*Book, error) {
	b, err := getBook(db, cid)
	if err != nil || b == nil {
		return b, err
	}
	if _, err := db.Exec(`DELETE FROM books WHERE cid = ?`, cid); err != nil {
		return nil, err
	}
	return b, nil
}
