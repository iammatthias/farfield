package main

import (
	"database/sql"
	"log/slog"
	"strings"

	"github.com/iammatthias/farfield/lib/store"
)

// Entries are content-addressed (the CID changes whenever the content does),
// so revisions come almost free: after every successful save the new state is
// snapshotted, deduplicated by CID, and capped per entry. Restoring is just
// another save — which itself becomes a revision, so nothing is ever lost by
// restoring.

const revisionSchema = `
CREATE TABLE IF NOT EXISTS entry_revisions (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
	cid      TEXT NOT NULL,
	title    TEXT NOT NULL,
	body     TEXT NOT NULL,
	saved_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS revisions_by_entry ON entry_revisions (entry_id, id DESC);`

// keepRevisions caps stored history per entry; older revisions are pruned.
const keepRevisions = 20

// Revision is one saved state of an entry.
type Revision struct {
	ID      int64
	EntryID int64
	CID     string
	Title   string
	Body    string
	SavedAt string
}

// saveRevision snapshots an entry's just-saved state. Best-effort by design:
// a failure here must never fail the save itself, so callers ignore the
// error and it logs instead. Consecutive identical states (same CID) are
// stored once.
func saveRevision(db *sql.DB, e *Entry) {
	id := e.ID
	if id == 0 {
		if err := db.QueryRow(`SELECT id FROM entries WHERE slug = ?`, e.Slug).Scan(&id); err != nil {
			slog.Warn("revision: resolve entry id", "slug", e.Slug, "err", err)
			return
		}
	}
	var lastCID string
	_ = db.QueryRow(
		`SELECT cid FROM entry_revisions WHERE entry_id = ? ORDER BY id DESC LIMIT 1`,
		id).Scan(&lastCID)
	if lastCID == e.CID {
		return
	}
	if _, err := db.Exec(
		`INSERT INTO entry_revisions (entry_id, cid, title, body, saved_at)
		 VALUES (?, ?, ?, ?, ?)`,
		id, e.CID, e.Title, e.Body, store.NowRFC3339()); err != nil {
		slog.Warn("revision: insert", "slug", e.Slug, "err", err)
		return
	}
	if _, err := db.Exec(
		`DELETE FROM entry_revisions WHERE entry_id = ? AND id NOT IN
		   (SELECT id FROM entry_revisions WHERE entry_id = ? ORDER BY id DESC LIMIT ?)`,
		id, id, keepRevisions); err != nil {
		slog.Warn("revision: prune", "slug", e.Slug, "err", err)
	}
}

// listRevisions returns an entry's saved states, newest first.
func listRevisions(db *sql.DB, entryID int64, limit int) ([]Revision, error) {
	rows, err := db.Query(
		`SELECT id, entry_id, cid, title, body, saved_at
		   FROM entry_revisions WHERE entry_id = ? ORDER BY id DESC LIMIT ?`,
		entryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		var r Revision
		if err := rows.Scan(&r.ID, &r.EntryID, &r.CID, &r.Title, &r.Body, &r.SavedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// getRevision loads one revision, including its body.
func getRevision(db *sql.DB, id int64) (*Revision, error) {
	var r Revision
	err := db.QueryRow(
		`SELECT id, entry_id, cid, title, body, saved_at
		   FROM entry_revisions WHERE id = ?`, id).
		Scan(&r.ID, &r.EntryID, &r.CID, &r.Title, &r.Body, &r.SavedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// revisionWords estimates a revision's size for display.
func revisionWords(body string) int { return len(strings.Fields(body)) }
