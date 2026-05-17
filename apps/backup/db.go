package main

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Backup is one recorded snapshot: which app it came from, the CID of the
// snapshot stored in blobs/R2, its size, and when it was taken.
type Backup struct {
	ID        int64  `json:"-"`
	App       string `json:"app"`
	CID       string `json:"cid"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"createdAt"`
}

const schema = `
CREATE TABLE IF NOT EXISTS backups (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	app        TEXT NOT NULL,
	cid        TEXT NOT NULL,
	size       INTEGER NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS backups_by_created ON backups (created_at DESC);`

const backupCols = `id, app, cid, size, created_at`

// openDB opens the SQLite database, applies pragmas, and migrates. It holds
// the backup registry and admin login sessions.
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
	return db, nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(...any) error }

func scanBackup(row scanner) (*Backup, error) {
	var b Backup
	if err := row.Scan(&b.ID, &b.App, &b.CID, &b.Size, &b.CreatedAt); err != nil {
		return nil, err
	}
	return &b, nil
}

// insertBackup records a snapshot and fills in its row id.
func insertBackup(db *sql.DB, b *Backup) error {
	res, err := db.Exec(
		`INSERT INTO backups (app, cid, size, created_at) VALUES (?, ?, ?, ?)`,
		b.App, b.CID, b.Size, b.CreatedAt)
	if err != nil {
		return err
	}
	b.ID, _ = res.LastInsertId()
	return nil
}

// listBackups returns every recorded snapshot, newest first.
func listBackups(db *sql.DB) ([]Backup, error) {
	rows, err := db.Query(
		`SELECT ` + backupCols + ` FROM backups ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// getBackup returns a snapshot record by id, or (nil, nil) if absent.
func getBackup(db *sql.DB, id int64) (*Backup, error) {
	b, err := scanBackup(db.QueryRow(
		`SELECT `+backupCols+` FROM backups WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// deleteBackup removes a snapshot record by id.
func deleteBackup(db *sql.DB, id int64) (bool, error) {
	res, err := db.Exec(`DELETE FROM backups WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// appHasCID reports whether the given app has already recorded a backup with
// this CID — i.e. this exact database state is already snapshotted in R2.
func appHasCID(db *sql.DB, app, cid string) (bool, error) {
	var one int
	err := db.QueryRow(
		`SELECT 1 FROM backups WHERE app = ? AND cid = ? LIMIT 1`, app, cid).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// cidReferenced reports whether any backup record still points at cid.
func cidReferenced(db *sql.DB, cid string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM backups WHERE cid = ? LIMIT 1`, cid).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// countBackups returns the total number of recorded snapshots.
func countBackups(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM backups`).Scan(&n)
	return n, err
}
