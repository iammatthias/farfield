package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// importSidecars copies every `<cid>.json` sidecar from the byte store into
// the SQLite metadata index. It is non-destructive — nothing in the store is
// modified. Existing rows are overwritten (the sidecar is the source of
// truth during migration).
func importSidecars(db *sql.DB, bs ByteStore) error {
	objects, err := bs.List()
	if err != nil {
		return fmt.Errorf("listing store: %w", err)
	}
	var sidecars, imported, failed int
	for _, obj := range objects {
		if !strings.HasSuffix(obj.Key, ".json") {
			continue
		}
		sidecars++
		cid := strings.TrimSuffix(obj.Key, ".json")

		data, err := bs.Get(obj.Key)
		if err != nil || data == nil {
			slog.Error("import: could not fetch sidecar", "key", obj.Key, "err", err)
			failed++
			continue
		}
		var m Meta
		if err := json.Unmarshal(data, &m); err != nil {
			slog.Error("import: could not parse sidecar", "key", obj.Key, "err", err)
			failed++
			continue
		}
		if m.CID == "" {
			m.CID = cid
		}
		if m.CreatedAt == "" {
			m.CreatedAt = obj.LastModified.UTC().Format(time.RFC3339)
		}
		if err := upsertMeta(db, &m); err != nil {
			slog.Error("import: could not upsert", "cid", m.CID, "err", err)
			failed++
			continue
		}
		imported++
	}
	slog.Info("import-sidecars complete",
		"sidecars_found", sidecars, "imported", imported, "failed", failed)
	if failed > 0 {
		return fmt.Errorf("%d sidecar(s) failed to import — fix and re-run before pruning", failed)
	}
	return nil
}

// pruneSidecars deletes `<cid>.json` sidecars from the byte store. It only
// deletes a sidecar whose metadata is already present in SQLite, so it can
// never orphan data. With confirm=false it is a dry run: it reports what it
// would delete and deletes nothing.
func pruneSidecars(db *sql.DB, bs ByteStore, confirm bool) error {
	objects, err := bs.List()
	if err != nil {
		return fmt.Errorf("listing store: %w", err)
	}
	var candidates, deleted, skipped int
	for _, obj := range objects {
		if !strings.HasSuffix(obj.Key, ".json") {
			continue
		}
		candidates++
		cid := strings.TrimSuffix(obj.Key, ".json")

		ok, err := metaExists(db, cid)
		if err != nil {
			return err
		}
		if !ok {
			slog.Warn("prune: SKIP — metadata not in DB, import it first", "key", obj.Key)
			skipped++
			continue
		}
		if !confirm {
			slog.Info("prune: would delete", "key", obj.Key)
			continue
		}
		if err := bs.Delete(obj.Key); err != nil {
			return fmt.Errorf("deleting %s: %w", obj.Key, err)
		}
		deleted++
	}
	if confirm {
		slog.Info("prune-sidecars complete",
			"candidates", candidates, "deleted", deleted, "skipped", skipped)
	} else {
		slog.Info("prune-sidecars DRY RUN — pass --confirm to delete",
			"candidates", candidates, "would_delete", candidates-skipped, "skipped", skipped)
	}
	return nil
}
