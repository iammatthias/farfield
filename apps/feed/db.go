package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Post is one ephemeral feed entry: a dated markdown note with tags. Links,
// images, anything else live inline in the markdown body. ID is the stable
// key; CID is the content hash — it changes whenever the content does.
type Post struct {
	ID        string   `json:"id"`
	CID       string   `json:"cid"`
	Body      string   `json:"body"`
	Tags      []string `json:"tags"`
	CreatedAt string   `json:"createdAt"`
	UpdatedAt string   `json:"updatedAt"`
}

const schema = `
CREATE TABLE IF NOT EXISTS posts (
	id         TEXT PRIMARY KEY,
	body       TEXT NOT NULL DEFAULT '',
	tags       TEXT NOT NULL DEFAULT '[]',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	cid        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS posts_by_created ON posts (created_at DESC);`

// postCols is the column list, in Post-field order, shared by every query.
const postCols = `id, cid, body, tags, created_at, updated_at`

// openDB opens the SQLite database, applies pragmas, and migrates.
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
	// Migrate databases created before CIDs: add the column, then backfill.
	if err := ensureColumn(db, "posts", "cid", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := backfillCIDs(db); err != nil {
		return nil, err
	}
	return db, nil
}

// ensureColumn adds a column to a table if it is not already present.
func ensureColumn(db *sql.DB, table, column, decl string) error {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + decl)
	return err
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

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

// postCID is the content identifier of a post: a CIDv1 over its canonical
// content — body and tags. The id (the key) and timestamps are excluded.
func postCID(p *Post) string {
	tags := p.Tags
	if tags == nil {
		tags = []string{}
	}
	return cid.OfValue(map[string]any{"body": p.Body, "tags": tags})
}

// backfillCIDs computes the content CID for any post that lacks one — a
// one-time migration for databases created before CIDs.
func backfillCIDs(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, body, tags FROM posts WHERE cid = ''`)
	if err != nil {
		return err
	}
	var posts []Post
	for rows.Next() {
		var p Post
		var tags string
		if err := rows.Scan(&p.ID, &p.Body, &tags); err != nil {
			rows.Close()
			return err
		}
		p.Tags = decodeTags(tags)
		posts = append(posts, p)
	}
	rows.Close()
	for i := range posts {
		if _, err := db.Exec(`UPDATE posts SET cid = ? WHERE id = ?`,
			postCID(&posts[i]), posts[i].ID); err != nil {
			return err
		}
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(...any) error }

func scanPost(row scanner) (*Post, error) {
	var p Post
	var tags string
	if err := row.Scan(&p.ID, &p.CID, &p.Body, &tags, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Tags = decodeTags(tags)
	return &p, nil
}

// listPosts returns every post, newest first.
func listPosts(db *sql.DB) ([]Post, error) {
	rows, err := db.Query(
		`SELECT ` + postCols + ` FROM posts ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// getPost returns a post by id, or (nil, nil) if absent.
func getPost(db *sql.DB, id string) (*Post, error) {
	p, err := scanPost(db.QueryRow(
		`SELECT `+postCols+` FROM posts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// insertPost creates a post, assigning a random id and timestamps.
func insertPost(db *sql.DB, p *Post) error {
	if p.ID == "" {
		p.ID = store.ShortID()
	}
	p.CreatedAt = nowRFC3339()
	p.UpdatedAt = p.CreatedAt
	p.CID = postCID(p)
	_, err := db.Exec(
		`INSERT INTO posts (id, cid, body, tags, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.ID, p.CID, p.Body, encodeTags(p.Tags), p.CreatedAt, p.UpdatedAt)
	return err
}

// updatePost replaces a post's body and tags, and stamps updated_at.
func updatePost(db *sql.DB, id string, p *Post) (bool, error) {
	p.CID = postCID(p)
	res, err := db.Exec(
		`UPDATE posts SET body = ?, tags = ?, cid = ?, updated_at = ? WHERE id = ?`,
		p.Body, encodeTags(p.Tags), p.CID, nowRFC3339(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// deletePost removes a post by id.
func deletePost(db *sql.DB, id string) (bool, error) {
	res, err := db.Exec(`DELETE FROM posts WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// importPost inserts or replaces a post, keyed by id, preserving the id and
// timestamps it carries — used by the vault importer.
func importPost(db *sql.DB, p *Post) error {
	if p.ID == "" {
		p.ID = store.ShortID()
	}
	if p.CreatedAt == "" {
		p.CreatedAt = nowRFC3339()
	}
	if p.UpdatedAt == "" {
		p.UpdatedAt = p.CreatedAt
	}
	p.CID = postCID(p)
	_, err := db.Exec(
		`INSERT INTO posts (id, cid, body, tags, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   cid=excluded.cid, body=excluded.body, tags=excluded.tags,
		   created_at=excluded.created_at, updated_at=excluded.updated_at`,
		p.ID, p.CID, p.Body, encodeTags(p.Tags), p.CreatedAt, p.UpdatedAt)
	return err
}
