package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/iammatthias/farfield/lib/backup"
	"github.com/iammatthias/farfield/lib/store"
)

// Snapshot retention is a grandfather-father-son ladder. Without one the
// registry grew without bound — fifteen apps × four snapshots a day, all
// retained, had put 51 GB of database copies in R2 on top of well under a
// gigabyte of actual data. The ladder keeps restore points dense where they
// are useful (the recent past) and sparse where they are ceremony (months
// ago):
//
//	≤ keepAllWindow     every snapshot
//	≤ keepDailyWindow   the newest snapshot per UTC day
//	≤ keepWeeklyWindow  the newest snapshot per ISO week
//	older               dropped
//
// The newest snapshot of each app is always kept, whatever its age — a
// retired app's last state stays restorable forever.
const (
	keepAllWindow    = 48 * time.Hour
	keepDailyWindow  = 30 * 24 * time.Hour
	keepWeeklyWindow = 90 * 24 * time.Hour
)

// prunable returns the snapshots that fall off the retention ladder, given
// every recorded snapshot newest-first (listBackups order). A snapshot whose
// created_at cannot be parsed is kept — never drop what cannot be dated.
func prunable(backups []Backup, now time.Time) []Backup {
	type seenSets struct {
		days   map[string]bool
		weeks  map[string]bool
		newest bool
	}
	seen := map[string]*seenSets{}
	var drop []Backup
	for _, b := range backups {
		s := seen[b.App]
		if s == nil {
			s = &seenSets{days: map[string]bool{}, weeks: map[string]bool{}}
			seen[b.App] = s
		}
		t, err := time.Parse(time.RFC3339, b.CreatedAt)
		if err != nil {
			continue // undatable — keep
		}
		day := t.UTC().Format("2006-01-02")
		year, week := t.UTC().ISOWeek()
		wk := fmt.Sprintf("%d-W%02d", year, week)
		age := now.Sub(t)

		// The first (newest) snapshot of an app is always kept; it still
		// claims its day and week so an older sibling can be dropped.
		if !s.newest {
			s.newest, s.days[day], s.weeks[wk] = true, true, true
			continue
		}
		switch {
		case age <= keepAllWindow:
			s.days[day], s.weeks[wk] = true, true
		case age <= keepDailyWindow && !s.days[day]:
			s.days[day], s.weeks[wk] = true, true
		case age <= keepWeeklyWindow && !s.weeks[wk]:
			s.weeks[wk] = true
		default:
			drop = append(drop, b)
		}
	}
	return drop
}

// pruneAll applies the retention ladder: registry rows first, then the
// snapshot bytes in blobs — and those only when no other record still points
// at the (content-addressed) CID, the same rule as the admin UI's delete.
// It returns how many snapshots were removed and their recorded size.
func pruneAll(db *sql.DB) (removed int, freed int64) {
	backups, err := listBackups(db)
	if err != nil {
		slog.Warn("prune: could not list backups", "err", err)
		return 0, 0
	}
	for _, b := range prunable(backups, time.Now().UTC()) {
		if _, err := deleteBackup(db, b.ID); err != nil {
			slog.Warn("prune: could not delete registry row",
				"app", b.App, "cid", b.CID, "err", err)
			continue
		}
		used, err := cidReferenced(db, b.CID)
		if err != nil {
			slog.Warn("prune: could not check snapshot references; keeping blob",
				"cid", b.CID, "err", err)
		} else if !used {
			if err := backup.Delete(blobsURL(), blobsKey(), b.CID); err != nil {
				slog.Warn("prune: could not delete snapshot from blobs",
					"cid", b.CID, "err", err)
			}
		}
		removed++
		freed += b.Size
	}
	return removed, freed
}

// runPrune is the `backup prune` CLI command. Without --confirm it is a dry
// run that prints what the ladder would drop; with --confirm it drops it.
// The scheduler applies the same ladder automatically after every snapshot
// cycle — this command exists to preview, and to reclaim space now rather
// than at the next tick.
func runPrune(confirm bool) error {
	db, err := openDB(store.Env("BACKUP_DB_PATH", "data/backup.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()

	if confirm {
		removed, freed := pruneAll(db)
		slog.Info("prune complete", "snapshots", removed, "freed", humanSize(freed))
		return nil
	}

	backups, err := listBackups(db)
	if err != nil {
		return err
	}
	drop := prunable(backups, time.Now().UTC())
	perApp := map[string]int{}
	var total int64
	for _, b := range drop {
		perApp[b.App]++
		total += b.Size
	}
	for app, n := range perApp {
		slog.Info("would prune", "app", app, "snapshots", n)
	}
	slog.Info("prune DRY RUN — pass --confirm to apply",
		"snapshots", len(drop), "size", humanSize(total))
	return nil
}
