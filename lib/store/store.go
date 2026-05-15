// Package store is the SQLite record store.
//
// It holds the current published state for one app service: the records,
// collections_meta, and deletions tables, plus the change_seq monotonic
// counter. No history — git is the history layer; change_seq exists only to
// give incremental consumers a safe cursor (a wall-clock timestamp is not).
//
// The store keeps a single SQLite connection (MaxOpenConns(1)): every
// operation serializes, which is correct and simple for a single-user
// backend and makes an in-memory database work for tests.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iammatthias/farfield/lib/core"
	_ "modernc.org/sqlite"
)

const migration = `
CREATE TABLE IF NOT EXISTS change_seq (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    next INTEGER NOT NULL
);
INSERT OR IGNORE INTO change_seq (id, next) VALUES (1, 1);

CREATE TABLE IF NOT EXISTS records (
    collection TEXT NOT NULL,
    rkey       TEXT NOT NULL,
    cid        TEXT NOT NULL,
    value      TEXT NOT NULL,   -- canonical JSON (hashes to cid)
    seq        INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (collection, rkey)
);
CREATE INDEX IF NOT EXISTS records_by_seq ON records (collection, seq);

CREATE TABLE IF NOT EXISTS collections_meta (
    collection TEXT PRIMARY KEY,
    list_etag  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS deletions (
    collection TEXT NOT NULL,
    rkey       TEXT NOT NULL,
    seq        INTEGER NOT NULL,
    deleted_at TEXT NOT NULL,
    PRIMARY KEY (collection, rkey)
);
CREATE INDEX IF NOT EXISTS deletions_by_seq ON deletions (collection, seq);
`

// Stored is a record as held by the store — the serving representation.
type Stored struct {
	Collection string         `json:"collection"`
	Rkey       string         `json:"rkey"`
	CID        string         `json:"cid"`
	Value      map[string]any `json:"value"`
	Seq        int64          `json:"seq"`
	CreatedAt  string         `json:"createdAt"`
	UpdatedAt  string         `json:"updatedAt"`
}

// Tombstone marks a deleted record so incremental consumers can evict it.
type Tombstone struct {
	Collection string `json:"collection"`
	Rkey       string `json:"rkey"`
	Seq        int64  `json:"seq"`
	DeletedAt  string `json:"deletedAt"`
}

// WriteResult is the outcome of a Put.
type WriteResult struct {
	// Status is "created", "updated", or "unchanged".
	Status string
	CID    string
	// Seq is the cursor value of the write; 0 when unchanged.
	Seq int64
}

// Store is a SQLite-backed record store.
type Store struct {
	db *sql.DB
}

// Open opens (creating if absent) a store at path.
func Open(path string) (*Store, error) {
	return openDSN(path)
}

// OpenInMemory opens an ephemeral store — for tests.
func OpenInMemory() (*Store, error) {
	return openDSN(":memory:")
}

func openDSN(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: serializes writes, and keeps :memory: coherent.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, err
	}
	if _, err := db.Exec(migration); err != nil {
		return nil, fmt.Errorf("migration: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Put creates or replaces a record. A submission whose canonical content
// hashes to the record's current CID is a no-op.
func (s *Store) Put(collection, rkey string, record core.Record) (WriteResult, error) {
	canonical, err := record.CanonicalBytes()
	if err != nil {
		return WriteResult{}, err
	}
	cid, err := record.CID()
	if err != nil {
		return WriteResult{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.Begin()
	if err != nil {
		return WriteResult{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	var existingCID, existingCreated string
	err = tx.QueryRow(
		"SELECT cid, created_at FROM records WHERE collection=? AND rkey=?",
		collection, rkey,
	).Scan(&existingCID, &existingCreated)
	existed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return WriteResult{}, err
	}
	if existed && existingCID == cid {
		return WriteResult{Status: "unchanged", CID: cid}, nil
	}

	seq, err := nextSeq(tx)
	if err != nil {
		return WriteResult{}, err
	}
	created := now
	if existed {
		created = existingCreated
	}
	if _, err := tx.Exec(
		`INSERT INTO records (collection, rkey, cid, value, seq, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(collection, rkey) DO UPDATE SET
		     cid=excluded.cid, value=excluded.value, seq=excluded.seq,
		     updated_at=excluded.updated_at`,
		collection, rkey, cid, string(canonical), seq, created, now,
	); err != nil {
		return WriteResult{}, err
	}
	// A recreate clears any prior tombstone — the rkey exists again.
	if _, err := tx.Exec("DELETE FROM deletions WHERE collection=? AND rkey=?", collection, rkey); err != nil {
		return WriteResult{}, err
	}
	if err := updateListETag(tx, collection); err != nil {
		return WriteResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return WriteResult{}, err
	}
	status := "created"
	if existed {
		status = "updated"
	}
	return WriteResult{Status: status, CID: cid, Seq: seq}, nil
}

// Delete removes a record and writes a tombstone. Reports whether one existed.
func (s *Store) Delete(collection, rkey string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.Exec("DELETE FROM records WHERE collection=? AND rkey=?", collection, rkey)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	seq, err := nextSeq(tx)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(
		`INSERT INTO deletions (collection, rkey, seq, deleted_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(collection, rkey) DO UPDATE SET seq=excluded.seq, deleted_at=excluded.deleted_at`,
		collection, rkey, seq, now,
	); err != nil {
		return false, err
	}
	if err := updateListETag(tx, collection); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Get fetches one record.
func (s *Store) Get(collection, rkey string) (*Stored, error) {
	row := s.db.QueryRow(
		"SELECT collection, rkey, cid, value, seq, created_at, updated_at FROM records WHERE collection=? AND rkey=?",
		collection, rkey,
	)
	st, err := scanStored(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return st, err
}

// List returns a collection's records, ordered by rkey.
func (s *Store) List(collection string) ([]Stored, error) {
	rows, err := s.db.Query(
		"SELECT collection, rkey, cid, value, seq, created_at, updated_at FROM records WHERE collection=? ORDER BY rkey",
		collection,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectStored(rows)
}

// ListETag returns a collection's list ETag — a hash of its sorted
// (rkey, cid) pairs. Empty string for an empty or unknown collection.
func (s *Store) ListETag(collection string) (string, error) {
	var etag string
	err := s.db.QueryRow("SELECT list_etag FROM collections_meta WHERE collection=?", collection).Scan(&etag)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return etag, err
}

// ChangedSince returns records and tombstones in collection with seq greater
// than since — the payload for an incremental ?since=<seq> fetch.
func (s *Store) ChangedSince(collection string, since int64) ([]Stored, []Tombstone, error) {
	rrows, err := s.db.Query(
		"SELECT collection, rkey, cid, value, seq, created_at, updated_at FROM records WHERE collection=? AND seq>? ORDER BY seq",
		collection, since,
	)
	if err != nil {
		return nil, nil, err
	}
	records, err := collectStored(rrows)
	rrows.Close()
	if err != nil {
		return nil, nil, err
	}
	drows, err := s.db.Query(
		"SELECT collection, rkey, seq, deleted_at FROM deletions WHERE collection=? AND seq>? ORDER BY seq",
		collection, since,
	)
	if err != nil {
		return nil, nil, err
	}
	defer drows.Close()
	var tombs []Tombstone
	for drows.Next() {
		var t Tombstone
		if err := drows.Scan(&t.Collection, &t.Rkey, &t.Seq, &t.DeletedAt); err != nil {
			return nil, nil, err
		}
		tombs = append(tombs, t)
	}
	return records, tombs, drows.Err()
}

// CurrentSeq returns the highest seq assigned so far — the cursor a ?since
// response hands back. 0 before any write.
func (s *Store) CurrentSeq() (int64, error) {
	var next int64
	err := s.db.QueryRow("SELECT next FROM change_seq WHERE id=1").Scan(&next)
	return next - 1, err
}

// ---------- internals ------------------------------------------------------

func nextSeq(tx *sql.Tx) (int64, error) {
	var seq int64
	if err := tx.QueryRow("SELECT next FROM change_seq WHERE id=1").Scan(&seq); err != nil {
		return 0, err
	}
	_, err := tx.Exec("UPDATE change_seq SET next = next + 1 WHERE id=1")
	return seq, err
}

// updateListETag recomputes a collection's list ETag inside the write
// transaction — a hash of sorted (rkey, cid) pairs, so an rkey moving is
// detected even when the set of CIDs is unchanged.
func updateListETag(tx *sql.Tx, collection string) error {
	rows, err := tx.Query("SELECT rkey, cid FROM records WHERE collection=? ORDER BY rkey", collection)
	if err != nil {
		return err
	}
	h := sha256.New()
	any := false
	for rows.Next() {
		var rkey, cid string
		if err := rows.Scan(&rkey, &cid); err != nil {
			rows.Close()
			return err
		}
		any = true
		h.Write([]byte(rkey))
		h.Write([]byte{0})
		h.Write([]byte(cid))
		h.Write([]byte{'\n'})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if any {
		_, err = tx.Exec(
			`INSERT INTO collections_meta (collection, list_etag) VALUES (?, ?)
			 ON CONFLICT(collection) DO UPDATE SET list_etag=excluded.list_etag`,
			collection, hex.EncodeToString(h.Sum(nil)),
		)
	} else {
		_, err = tx.Exec("DELETE FROM collections_meta WHERE collection=?", collection)
	}
	return err
}

type scannable interface {
	Scan(dest ...any) error
}

func scanStored(row scannable) (*Stored, error) {
	var st Stored
	var value string
	if err := row.Scan(&st.Collection, &st.Rkey, &st.CID, &value, &st.Seq, &st.CreatedAt, &st.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(value), &st.Value); err != nil {
		return nil, fmt.Errorf("corrupt stored json for %s/%s: %w", st.Collection, st.Rkey, err)
	}
	return &st, nil
}

func collectStored(rows *sql.Rows) ([]Stored, error) {
	var out []Stored
	for rows.Next() {
		st, err := scanStored(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}
