package main

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Book is one EPUB in the catalog. The CID is the content identifier of the
// EPUB bytes; CoverCID, when set, is the CID of the extracted cover image and
// ThumbCID the CID of its downscaled JPEG thumbnail. All are keys into the
// ByteStore, where the bytes themselves live.
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
	ThumbCID    string `json:"thumbCid"`
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
	thumb_cid   TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS books_by_created ON books (created_at DESC, cid);`

// bookCols is the column list, in Book-field order, shared by every query.
const bookCols = `cid, title, author, language, identifier, description, collection, filename, size, cover_cid, cover_mime, thumb_cid, created_at`

// openDB opens the SQLite database, applies pragmas, and migrates. It holds the
// book metadata index and admin login sessions; book and cover bytes live in
// the ByteStore.
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
	if _, err := db.Exec(uploadsSchema); err != nil {
		return nil, err
	}
	// Self-migrate: add the collection column to databases created before it
	// existed, then index it (the index can only be built once the column is).
	if err := store.EnsureColumn(db, "books", "collection", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	// Self-migrate: cover thumbnails arrived after the first deployments.
	if err := store.EnsureColumn(db, "books", "thumb_cid", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if _, err := db.Exec(
		`CREATE INDEX IF NOT EXISTS books_by_collection ON books (collection, created_at DESC)`); err != nil {
		return nil, err
	}
	return db, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(...any) error }

func scanBook(row scanner) (*Book, error) {
	var b Book
	err := row.Scan(&b.CID, &b.Title, &b.Author, &b.Language, &b.Identifier,
		&b.Description, &b.Collection, &b.Filename, &b.Size, &b.CoverCID, &b.CoverMime,
		&b.ThumbCID, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// upsertBook inserts a book's metadata, replacing any existing row for the same
// CID (re-uploading identical bytes is idempotent). Identity fields —
// created_at and collection — persist across re-uploads so a book keeps its
// place in "newest first" feeds and its folder.
func upsertBook(db *sql.DB, b *Book) error {
	_, err := db.Exec(
		`INSERT INTO books (`+bookCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(cid) DO UPDATE SET
		   title=excluded.title, author=excluded.author, language=excluded.language,
		   identifier=excluded.identifier, description=excluded.description,
		   filename=excluded.filename, size=excluded.size,
		   cover_cid=excluded.cover_cid, cover_mime=excluded.cover_mime,
		   thumb_cid=excluded.thumb_cid`,
		b.CID, b.Title, b.Author, b.Language, b.Identifier, b.Description,
		b.Collection, b.Filename, b.Size, b.CoverCID, b.CoverMime, b.ThumbCID, b.CreatedAt)
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

// listBooks returns every book's metadata, newest first (the OPDS "All books"
// feed).
func listBooks(db *sql.DB) ([]Book, error) {
	rows, err := db.Query(
		`SELECT ` + bookCols + ` FROM books ORDER BY created_at DESC, cid`)
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

// catalogVersion returns a cheap fingerprint of the catalog — the book count
// plus the newest created_at stamp — used as the OPDS feeds' ETag so unchanged
// catalogs revalidate with a 304 instead of a full XML rebuild.
func catalogVersion(db *sql.DB) (string, error) {
	var v string
	err := db.QueryRow(
		`SELECT COUNT(*) || '-' || COALESCE(MAX(created_at),'') FROM books`).Scan(&v)
	return v, err
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

// updateBooksCollection moves the given books into a collection with one
// UPDATE, rather than a statement per book. Unknown CIDs are simply ignored.
func updateBooksCollection(db *sql.DB, cids []string, collection string) error {
	if len(cids) == 0 {
		return nil
	}
	args := make([]any, 0, len(cids)+1)
	args = append(args, collection)
	for _, c := range cids {
		args = append(args, c)
	}
	placeholders := strings.Repeat(",?", len(cids))[1:]
	_, err := db.Exec(
		`UPDATE books SET collection = ? WHERE cid IN (`+placeholders+`)`, args...)
	return err
}

// coverInfo reports whether any book references id as its cover image or
// cover thumbnail, and the image's MIME type (thumbnails are always JPEG).
// It backs both the cover endpoint (an image is only served when it belongs
// to a book) and delete cleanup (an image's bytes are only removed once no
// book references them).
func coverInfo(db *sql.DB, id string) (mime string, exists bool, err error) {
	err = db.QueryRow(
		`SELECT CASE WHEN cover_cid = ? THEN cover_mime ELSE 'image/jpeg' END
		 FROM books WHERE cover_cid = ? OR thumb_cid = ? LIMIT 1`, id, id, id).Scan(&mime)
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

// uploadsSchema tracks in-progress resumable (tus) uploads. The partial bytes
// live in a staging file on disk keyed by id; this row carries the declared
// length and the EPUB's eventual filename/collection until the upload completes
// and becomes a book (or is pruned for going stale).
const uploadsSchema = `
CREATE TABLE IF NOT EXISTS uploads (
	id         TEXT PRIMARY KEY,
	length     INTEGER NOT NULL,
	filename   TEXT NOT NULL DEFAULT '',
	collection TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);`

// Upload is one in-progress resumable upload.
type Upload struct {
	ID         string
	Length     int64
	Filename   string
	Collection string
	CreatedAt  string
}

func createUpload(db *sql.DB, u *Upload) error {
	_, err := db.Exec(
		`INSERT INTO uploads (id, length, filename, collection, created_at) VALUES (?, ?, ?, ?, ?)`,
		u.ID, u.Length, u.Filename, u.Collection, u.CreatedAt)
	return err
}

func getUpload(db *sql.DB, id string) (*Upload, error) {
	var u Upload
	err := db.QueryRow(
		`SELECT id, length, filename, collection, created_at FROM uploads WHERE id = ?`, id).
		Scan(&u.ID, &u.Length, &u.Filename, &u.Collection, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func deleteUpload(db *sql.DB, id string) error {
	_, err := db.Exec(`DELETE FROM uploads WHERE id = ?`, id)
	return err
}

// pruneUploads deletes upload rows created before the cutoff and returns their
// ids so the caller can remove the matching staging files.
func pruneUploads(db *sql.DB, cutoff string) ([]string, error) {
	rows, err := db.Query(`SELECT id FROM uploads WHERE created_at < ?`, cutoff)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, nil
	}
	_, err = db.Exec(`DELETE FROM uploads WHERE created_at < ?`, cutoff)
	return ids, err
}
