package main

import (
	"database/sql"
	"errors"

	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// schema is the blob metadata index. The created_at index backs the admin
// UI's reverse-chronological pagination.
const schema = `
CREATE TABLE IF NOT EXISTS blobs (
	cid            TEXT PRIMARY KEY,
	mime           TEXT NOT NULL,
	size           INTEGER NOT NULL,
	width          INTEGER NOT NULL DEFAULT 0,
	height         INTEGER NOT NULL DEFAULT 0,
	blurhash       TEXT NOT NULL DEFAULT '',
	dominant_color TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS blobs_by_created ON blobs (created_at DESC, cid);`

// thumbIndex backs the thumbnail reference counting done on delete and when
// serving thumbnails. Created after EnsureColumn so old databases have the
// column first.
const thumbIndex = `CREATE INDEX IF NOT EXISTS blobs_by_thumb ON blobs (thumb_cid);`

// blobCols is the column list, in Meta-field order, shared by every query.
const blobCols = `cid, size, mime, width, height, blurhash, dominant_color, thumb_cid, created_at`

// openDB opens the SQLite database, applies pragmas, and migrates. It holds
// the blob metadata index and admin login sessions; blob bytes live in the
// ByteStore.
func openDB(path string) (*sql.DB, error) {
	db, err := store.OpenDB(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// thumb_cid arrived after the first deployments — bring old databases
	// current before creating the index that depends on it.
	if err := store.EnsureColumn(db, "blobs", "thumb_cid", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if _, err := db.Exec(thumbIndex); err != nil {
		return nil, err
	}
	if _, err := db.Exec(store.SessionSchema); err != nil {
		return nil, err
	}
	return db, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(...any) error }

func scanMeta(row scanner) (*Meta, error) {
	var m Meta
	err := row.Scan(&m.CID, &m.Size, &m.Mime, &m.Width, &m.Height,
		&m.Blurhash, &m.DominantColor, &m.ThumbCID, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// upsertMeta inserts a blob's metadata, replacing any existing row. A conflict
// keeps the existing thumb_cid when the incoming row has none, so a sidecar
// re-import never orphans an already-generated thumbnail.
func upsertMeta(db *sql.DB, m *Meta) error {
	_, err := db.Exec(
		`INSERT INTO blobs (`+blobCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(cid) DO UPDATE SET
		   size=excluded.size, mime=excluded.mime, width=excluded.width,
		   height=excluded.height, blurhash=excluded.blurhash,
		   dominant_color=excluded.dominant_color,
		   thumb_cid=CASE WHEN excluded.thumb_cid = ''
		     THEN blobs.thumb_cid ELSE excluded.thumb_cid END`,
		m.CID, m.Size, m.Mime, m.Width, m.Height,
		m.Blurhash, m.DominantColor, m.ThumbCID, m.CreatedAt)
	return err
}

// getMeta returns a blob's metadata, or (nil, nil) when no such CID exists.
func getMeta(db *sql.DB, cid string) (*Meta, error) {
	m, err := scanMeta(db.QueryRow(
		`SELECT `+blobCols+` FROM blobs WHERE cid = ?`, cid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// listMeta returns a page of blob metadata, newest first.
func listMeta(db *sql.DB, limit, offset int) ([]Meta, error) {
	rows, err := db.Query(
		`SELECT `+blobCols+` FROM blobs ORDER BY created_at DESC, cid LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Meta
	for rows.Next() {
		m, err := scanMeta(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// countMeta returns the total number of blobs in the index.
func countMeta(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&n)
	return n, err
}

// metaExists reports whether a blob's metadata is in the index.
func metaExists(db *sql.DB, cid string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM blobs WHERE cid = ?`, cid).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// deleteMeta removes a blob's metadata. It reports whether a row existed and,
// when one did, the row's thumb CID so the caller can clean up the thumbnail
// bytes.
func deleteMeta(db *sql.DB, cid string) (existed bool, thumbCID string, err error) {
	err = db.QueryRow(
		`DELETE FROM blobs WHERE cid = ? RETURNING thumb_cid`, cid).Scan(&thumbCID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, thumbCID, nil
}

// thumbRefCount counts live references to cid as stored bytes: blob rows
// using it as their thumbnail, plus any media row whose own bytes it is.
// Identical pixel content thumbnails dedupe to one CID, so a thumbnail may
// be shared — its bytes are only safe to delete when this reaches zero.
func thumbRefCount(db *sql.DB, cid string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM blobs WHERE thumb_cid = ?) +
		(SELECT COUNT(*) FROM blobs WHERE cid = ?)`, cid, cid).Scan(&n)
	return n, err
}
