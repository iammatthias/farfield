package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iammatthias/farfield/lib/backup"
	"github.com/iammatthias/farfield/lib/store"
)

// appTarget is a farfield app whose database the backup app snapshots.
type appTarget struct {
	Name   string
	DBPath string
}

// targets resolves the apps to back up by scanning the shared data directory
// for *.sqlite files. Every farfield app keeps its database beside the backup
// app's own in /data, so a newly deployed app (calendar, bookmarks, qr, ...) is
// picked up automatically with no backup-side configuration to keep in sync.
// The backup registry itself is skipped; SQLite -wal/-shm sidecars never match
// the *.sqlite glob. apex has no database, so it simply never appears.
func targets() []appTarget {
	dataDir := filepath.Dir(store.Env("BACKUP_DB_PATH", "data/backup.sqlite"))
	matches, _ := filepath.Glob(filepath.Join(dataDir, "*.sqlite"))
	sort.Strings(matches)
	out := make([]appTarget, 0, len(matches))
	for _, p := range matches {
		name := strings.TrimSuffix(filepath.Base(p), ".sqlite")
		if name == "backup" {
			continue // the backup app's own registry, not app data
		}
		out = append(out, appTarget{Name: name, DBPath: p})
	}
	return out
}

// snapResult is the outcome of snapshotting one app.
type snapResult struct {
	App     string
	CID     string
	Size    int64
	Skipped bool // database unchanged since a prior backup — nothing uploaded
	Err     string
}

// snapshotAll snapshots every target app's database. It never aborts on a
// single failure — every app is attempted.
func snapshotAll(db *sql.DB) []snapResult {
	blobsURL := store.Env("BLOBS_URL", "http://127.0.0.1:8789")
	apiKey := store.Env("BLOBS_API_KEY", "")
	var out []snapResult
	for _, t := range targets() {
		r := snapResult{App: t.Name}
		if _, err := os.Stat(t.DBPath); err != nil {
			r.Err = "database not found: " + t.DBPath
			out = append(out, r)
			continue
		}
		cid, size, skipped, err := snapshotOne(db, t, blobsURL, apiKey)
		if err != nil {
			r.Err = err.Error()
		} else {
			r.CID, r.Size, r.Skipped = cid, size, skipped
		}
		out = append(out, r)
	}
	return out
}

// snapshotOne snapshots a single app's database. The snapshot is content-
// addressed: if its CID is one this app has already backed up — the database
// has not changed — nothing is uploaded and no row is recorded.
func snapshotOne(db *sql.DB, t appTarget, blobsURL, apiKey string) (cid string, size int64, skipped bool, err error) {
	appDB, err := sql.Open("sqlite", "file:"+t.DBPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return "", 0, false, err
	}
	defer appDB.Close()

	data, err := backup.Snapshot(appDB)
	if err != nil {
		return "", 0, false, err
	}
	cid = backup.CID(data)

	has, err := appHasCID(db, t.Name, cid)
	if err != nil {
		return "", 0, false, err
	}
	if has {
		// Unchanged — this exact snapshot is already in R2 and on record.
		return cid, int64(len(data)), true, nil
	}

	if _, err := backup.Push(blobsURL, apiKey, data); err != nil {
		return "", 0, false, err
	}
	rec := &Backup{App: t.Name, CID: cid, Size: int64(len(data)), CreatedAt: nowRFC3339()}
	if err := insertBackup(db, rec); err != nil {
		return "", 0, false, err
	}
	return cid, rec.Size, false, nil
}

// runSnapshot is the `backup snapshot` CLI command — the one a server-side
// scheduler runs on a timer.
func runSnapshot() error {
	db, err := openDB(store.Env("BACKUP_DB_PATH", "data/backup.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	var failed int
	for _, r := range snapshotAll(db) {
		switch {
		case r.Err != "":
			slog.Error("snapshot failed", "app", r.App, "err", r.Err)
			failed++
		case r.Skipped:
			slog.Info("unchanged — skipped", "app", r.App, "cid", r.CID)
		default:
			slog.Info("snapshot", "app", r.App, "cid", r.CID, "size", r.Size)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d app(s) failed to snapshot", failed)
	}
	return nil
}

// runRestore is the `backup restore` CLI command. Without --confirm it is a
// dry run. With --confirm it takes a pre-restore safety snapshot, then
// overwrites the target app's database — that app must be stopped first.
func runRestore(app, cid string, confirm bool) error {
	tgts := targets()
	var target *appTarget
	for _, t := range tgts {
		if t.Name == app {
			t := t
			target = &t
			break
		}
	}
	if target == nil {
		avail := make([]string, len(tgts))
		for i, t := range tgts {
			avail[i] = t.Name
		}
		return fmt.Errorf("unknown app %q (available: %s)", app, strings.Join(avail, ", "))
	}

	db, err := openDB(store.Env("BACKUP_DB_PATH", "data/backup.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	blobsURL := store.Env("BLOBS_URL", "http://127.0.0.1:8789")
	apiKey := store.Env("BLOBS_API_KEY", "")

	data, err := backup.Pull(blobsURL, apiKey, cid)
	if err != nil {
		return err
	}
	if !confirm {
		slog.Info("restore DRY RUN — pass --confirm to apply",
			"app", app, "cid", cid, "bytes", len(data), "target", target.DBPath)
		return nil
	}

	slog.Warn("restoring — stop the target service before running this", "app", app)

	// Safety net: snapshot the current database before overwriting it.
	if _, err := os.Stat(target.DBPath); err == nil {
		if scid, _, _, e := snapshotOne(db, *target, blobsURL, apiKey); e == nil {
			slog.Info("current state is safe", "cid", scid)
		} else {
			slog.Warn("could not take a pre-restore safety snapshot", "err", e)
		}
	}

	if err := backup.WriteDB(target.DBPath, data); err != nil {
		return err
	}
	slog.Info("restore complete — start the target service again",
		"app", app, "target", target.DBPath)
	return nil
}
