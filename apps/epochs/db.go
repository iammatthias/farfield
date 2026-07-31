package main

import (
	"database/sql"
	"encoding/json"

	"github.com/iammatthias/farfield/lib/store"
)

// openDB opens the app's SQLite file and brings its schema current. Following
// the farfield convention, the schema migrates itself on startup: shipping new
// code is the whole migration step.
//
// The database holds one row. Epochs is a read-only view of a public contract,
// so there is nothing to persist except the last reading we trusted — which
// lets a restart during an RPC outage render real numbers instead of "Loading".
func openDB(path string) (*sql.DB, error) {
	db, err := store.OpenDB(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS snapshot (
			id         INTEGER PRIMARY KEY CHECK (id = 1),
			block      INTEGER NOT NULL,
			labels     TEXT    NOT NULL,
			updated_at TEXT    NOT NULL
		)`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// saveSnapshot records the latest good reading. The epoch values are not
// stored: they are a pure function of the block number, so keeping them would
// only create a way for the two to disagree.
func saveSnapshot(db *sql.DB, block uint64, labels [Count]string) error {
	encoded, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		INSERT INTO snapshot (id, block, labels, updated_at) VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			block = excluded.block,
			labels = excluded.labels,
			updated_at = excluded.updated_at`,
		block, string(encoded), store.NowRFC3339())
	return err
}

// loadSnapshot returns the stored reading. ok is false when the table is empty
// — a first run, which simply means the page waits for the chain.
func loadSnapshot(db *sql.DB) (block uint64, labels [Count]string, ok bool) {
	var encoded string
	err := db.QueryRow(`SELECT block, labels FROM snapshot WHERE id = 1`).Scan(&block, &encoded)
	if err != nil {
		return 0, DefaultLabels, false
	}
	labels = DefaultLabels
	// A labels column that fails to decode is not worth failing a page render
	// over; the defaults are the same names the contract has always held.
	_ = json.Unmarshal([]byte(encoded), &labels)
	return block, labels, true
}
