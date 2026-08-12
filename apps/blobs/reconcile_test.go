package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReferencedCIDsRefusesPartialSet: a missing sibling database aborts —
// reconciling without the library's rows would read every book as garbage.
func TestReferencedCIDsRefusesPartialSet(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "blobs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := referencedCIDs(db, dir); err == nil {
		t.Fatal("reconcile proceeded without the sibling databases")
	}
}

// TestReconcileFindsOrphans drives the whole census over the local backend:
// referenced objects (media, thumbs, books, covers, snapshots) survive, an
// unreferenced old object is the one orphan, and a fresh unreferenced object
// stays inside the grace window.
func TestReconcileFindsOrphans(t *testing.T) {
	dir := t.TempDir()

	// Own index: one blob with a thumbnail.
	db, err := openDB(filepath.Join(dir, "blobs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO blobs (cid, mime, size, created_at, thumb_cid)
		VALUES ('media1', 'image/png', 3, '2026-01-01T00:00:00Z', 'thumb1')`); err != nil {
		t.Fatal(err)
	}

	// Siblings: a library book with cover, and a backup snapshot.
	lib, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "library.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Exec(`CREATE TABLE books (cid TEXT, cover_cid TEXT DEFAULT '', thumb_cid TEXT DEFAULT '');
		INSERT INTO books VALUES ('book1', 'cover1', '')`); err != nil {
		t.Fatal(err)
	}
	lib.Close()
	bk, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "backup.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bk.Exec(`CREATE TABLE backups (cid TEXT);
		INSERT INTO backups VALUES ('snap1')`); err != nil {
		t.Fatal(err)
	}
	bk.Close()
	sl, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "sideload.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sl.Exec(`CREATE TABLE builds (cid TEXT DEFAULT '');
		CREATE TABLE app_screenshots (cid TEXT, ext TEXT DEFAULT '');
		INSERT INTO builds VALUES ('ipa1');
		INSERT INTO app_screenshots VALUES ('shot1', '.png')`); err != nil {
		t.Fatal(err)
	}
	sl.Close()

	refs, err := referencedCIDs(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"media1", "thumb1", "book1", "cover1", "snap1",
		"ipa1.ipa", "shot1.png"} {
		if !refs[want] {
			t.Errorf("reference set missing %q", want)
		}
	}

	// A store holding everything referenced plus one stale orphan and one
	// fresh (in-grace) unreferenced object.
	bs, err := OpenLocalDir(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"media1", "thumb1", "book1", "cover1", "snap1", "orphan1", "fresh1"} {
		if err := bs.Put(k, []byte("x"), ""); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * reconcileGrace)
	if err := os.Chtimes(filepath.Join(dir, "store", "orphan1"), old, old); err != nil {
		t.Fatal(err)
	}

	objects, err := bs.List()
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-reconcileGrace)
	var orphans []string
	for _, o := range objects {
		if !refs[o.Key] && !o.LastModified.After(cutoff) {
			orphans = append(orphans, o.Key)
		}
	}
	if len(orphans) != 1 || orphans[0] != "orphan1" {
		t.Fatalf("orphans = %v, want [orphan1]", orphans)
	}
}
