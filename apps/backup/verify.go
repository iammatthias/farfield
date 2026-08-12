package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/iammatthias/farfield/lib/backup"
	"github.com/iammatthias/farfield/lib/store"
)

// The restore drill, minus the restore. A backup that has never been read
// back is a hope, not a backup: `backup verify` pulls the newest snapshot of
// an app (or of every app) from blobs/R2 into a temp file, opens it, and has
// SQLite prove it — PRAGMA integrity_check across every page, plus a table
// census so a structurally-valid-but-empty file cannot pass silently. The
// snapshot streams to disk and is removed after; nothing running is touched.

// verifySnapshot opens the SQLite file at path read-only and checks it:
// integrity_check must answer "ok" and the schema must contain at least one
// table. Returns the table count.
func verifySnapshot(path string) (tables int, err error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer db.Close()

	var verdict string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&verdict); err != nil {
		return 0, fmt.Errorf("integrity_check: %w", err)
	}
	if verdict != "ok" {
		return 0, fmt.Errorf("integrity_check: %s", verdict)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table'`).Scan(&tables); err != nil {
		return 0, err
	}
	if tables == 0 {
		return 0, fmt.Errorf("snapshot has no tables")
	}
	return tables, nil
}

// newestSnapshots returns the newest recorded snapshot per app, optionally
// filtered to one app.
func newestSnapshots(db *sql.DB, app string) ([]Backup, error) {
	q := `SELECT ` + backupCols + ` FROM backups b
		WHERE created_at = (SELECT MAX(created_at) FROM backups WHERE app = b.app)`
	args := []any{}
	if app != "" {
		q += ` AND app = ?`
		args = append(args, app)
	}
	q += ` ORDER BY app`
	rows, err := db.Query(q, args...)
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

// runVerify is the `backup verify [app]` CLI command.
func runVerify(app string) error {
	db, err := openDB(store.Env("BACKUP_DB_PATH", "data/backup.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	snaps, err := newestSnapshots(db, app)
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		return fmt.Errorf("no snapshots recorded%s", map[bool]string{true: " for " + app}[app != ""])
	}

	blobsURL := store.Env("BLOBS_URL", "http://127.0.0.1:8789")
	apiKey := store.Env("BLOBS_API_KEY", "")
	var failed int
	for _, b := range snaps {
		tmp, err := os.CreateTemp(os.TempDir(), "verify-*.sqlite")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		err = backup.Pull(blobsURL, apiKey, b.CID, tmp)
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			var tables int
			if tables, err = verifySnapshot(tmpName); err == nil {
				slog.Info("verified", "app", b.App, "cid", b.CID,
					"size", humanSize(b.Size), "tables", tables, "taken", b.CreatedAt)
			}
		}
		if err != nil {
			slog.Error("verify FAILED", "app", b.App, "cid", b.CID, "err", err)
			failed++
		}
		_ = os.Remove(tmpName)
		// -wal/-shm never exist for a fresh ro open, but cost nothing to sweep.
		_ = os.Remove(filepath.Clean(tmpName) + "-wal")
		_ = os.Remove(filepath.Clean(tmpName) + "-shm")
	}
	if failed > 0 {
		return fmt.Errorf("%d snapshot(s) failed verification", failed)
	}
	slog.Info("every latest snapshot restores and passes integrity_check",
		"apps", len(snaps))
	return nil
}
