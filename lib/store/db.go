package store

import (
	"database/sql"
	"fmt"
	"time"
)

// OpenDB opens the SQLite database at path with farfield's standard pragmas
// and pool settings, and verifies the connection. The calling module must
// import the driver (_ "modernc.org/sqlite") — this package stays
// standard-library only.
//
// Pragmas: WAL for concurrent readers, busy_timeout to absorb write
// contention, synchronous=NORMAL (the canonical WAL setting — durable except
// against power loss of the very last transactions, one fsync cheaper per
// commit), and foreign_keys on. The small connection pool keeps WAL writer
// contention in-process instead of burning the busy timeout.
func OpenDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"+
			"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)",
		path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// EnsureColumn adds a column to a table if it is not already present. The
// table/column/decl are code constants, so the string-built DDL is safe.
func EnsureColumn(db *sql.DB, table, column, decl string) error {
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

// RenameColumn renames a table column when the old name still exists and the
// new one does not — a one-time migration for databases predating a rename.
func RenameColumn(db *sql.DB, table, oldName, newName string) error {
	var hasOld, hasNew int
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?),
		(SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?)`,
		table, oldName, table, newName).Scan(&hasOld, &hasNew); err != nil {
		return err
	}
	if hasOld == 0 || hasNew > 0 {
		return nil
	}
	_, err := db.Exec("ALTER TABLE " + table + " RENAME COLUMN " + oldName + " TO " + newName)
	return err
}

// NowRFC3339 returns the current UTC time in RFC 3339 — the timestamp format
// farfield stores in SQLite TEXT columns.
func NowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
