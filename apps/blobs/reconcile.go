package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/iammatthias/farfield/lib/store"
)

// `blobs reconcile` closes the byte-store's one open loop: deletes whose
// blob-removal leg failed after the registry leg succeeded (the prune and
// the admin delete both warn-and-continue there) leave orphaned objects no
// row points at — invisible, unbilled-for-nothing bytes. Reconcile lists the
// store and subtracts every CID something still references:
//
//   - this service's own media index (blobs + their generated thumbnails)
//   - the library's books, covers, and thumbnails (read-only, sibling DB)
//   - the backup registry's snapshots (read-only, sibling DB)
//
// What remains is orphaned. Dry-run by default; --confirm deletes.
//
// Two safety properties matter more than thoroughness. First, a missing or
// unreadable sibling database aborts the run — reconciling with a partial
// reference set would classify everything that database references as
// garbage. Second, objects younger than reconcileGrace are never touched:
// every writer stores bytes first and commits its row second, so a fresh
// object can be legitimately row-less for a moment.
const reconcileGrace = time.Hour

// referencedCIDs gathers every CID the fleet still points at.
func referencedCIDs(ownDB *sql.DB, dataDir string) (map[string]bool, error) {
	refs := map[string]bool{}

	collect := func(db *sql.DB, query string) error {
		rows, err := db.Query(query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cid string
			if err := rows.Scan(&cid); err != nil {
				return err
			}
			if cid != "" {
				refs[cid] = true
			}
		}
		return rows.Err()
	}

	if err := collect(ownDB, `SELECT cid FROM blobs`); err != nil {
		return nil, fmt.Errorf("own media index: %w", err)
	}
	if err := collect(ownDB, `SELECT thumb_cid FROM blobs WHERE thumb_cid != ''`); err != nil {
		return nil, fmt.Errorf("own thumbnails: %w", err)
	}

	// Sibling databases, read-only. Every farfield app keeps its database in
	// the shared data directory; absence is a reason to stop, not to shrug —
	// without the library's rows every book would read as an orphan.
	for _, sib := range []struct {
		file    string
		queries []string
	}{
		{"library.sqlite", []string{
			`SELECT cid FROM books`,
			`SELECT cover_cid FROM books WHERE cover_cid != ''`,
			`SELECT thumb_cid FROM books WHERE thumb_cid != ''`,
		}},
		{"backup.sqlite", []string{
			`SELECT cid FROM backups`,
		}},
		// Sideload's objects carry their extension in the key (<cid>.ipa,
		// <cid>.png) — the concatenation below mirrors its store layout.
		{"sideload.sqlite", []string{
			`SELECT cid || '.ipa' FROM builds WHERE cid != ''`,
			`SELECT cid || ext FROM app_screenshots WHERE cid != ''`,
		}},
	} {
		path := filepath.Join(dataDir, sib.file)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("refusing to reconcile without %s: %w", sib.file, err)
		}
		db, err := sql.Open("sqlite",
			"file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", sib.file, err)
		}
		for _, q := range sib.queries {
			if err := collect(db, q); err != nil {
				db.Close()
				return nil, fmt.Errorf("%s: %w", sib.file, err)
			}
		}
		db.Close()
	}
	return refs, nil
}

// runReconcile is the `blobs reconcile [--confirm]` CLI command.
func runReconcile(confirm bool) error {
	dbPath := store.Env("BLOBS_DB_PATH", "blobs.sqlite")
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	bs, desc, err := openStore()
	if err != nil {
		return err
	}
	slog.Info("reconciling byte store", "store", desc)

	objects, err := bs.List()
	if err != nil {
		return fmt.Errorf("list store: %w", err)
	}
	refs, err := referencedCIDs(db, filepath.Dir(dbPath))
	if err != nil {
		return err
	}

	var orphans []ObjectInfo
	var orphanBytes int64
	var fresh int
	cutoff := time.Now().Add(-reconcileGrace)
	for _, o := range objects {
		if refs[o.Key] {
			continue
		}
		if o.LastModified.After(cutoff) {
			fresh++ // possibly a store-then-commit write in flight — never touch
			continue
		}
		orphans = append(orphans, o)
		orphanBytes += o.Size
	}

	slog.Info("reconcile census", "objects", len(objects), "referenced", len(refs),
		"orphans", len(orphans), "orphan_bytes", humanSize(orphanBytes),
		"in_grace_window", fresh)
	for _, o := range orphans {
		slog.Info("orphan", "cid", o.Key, "size", humanSize(o.Size),
			"modified", o.LastModified.UTC().Format(time.RFC3339))
	}

	if !confirm {
		if len(orphans) > 0 {
			slog.Info("dry run — pass --confirm to delete the orphans")
		}
		return nil
	}
	var failed int
	for _, o := range orphans {
		if err := bs.Delete(o.Key); err != nil {
			slog.Error("could not delete orphan", "cid", o.Key, "err", err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d orphan(s) could not be deleted", failed)
	}
	slog.Info("reconcile complete", "deleted", len(orphans), "freed", humanSize(orphanBytes))
	return nil
}
