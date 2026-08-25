package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iammatthias/farfield/lib/store"
)

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		2048:    "2.0 KB",
		5 << 20: "5.0 MB",
	}
	for n, want := range cases {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTargets(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{
		"content.sqlite", "feed.sqlite", "blobs.sqlite",
		"daily.sqlite", "bookmarks.sqlite", "qr.sqlite", "backup.sqlite",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a WAL sidecar must not be mistaken for an app database
	if err := os.WriteFile(filepath.Join(dir, "content.sqlite-wal"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// nor may the pulse/ telemetry sidecars — request logs are rolling
	// telemetry, deliberately outside the app databases so snapshots stay
	// content-stable; backing them up would put the churn right back.
	if err := os.MkdirAll(filepath.Join(dir, "pulse"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pulse", "qr.sqlite"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BACKUP_DB_PATH", filepath.Join(dir, "backup.sqlite"))

	names := map[string]bool{}
	for _, tg := range targets() {
		if tg.Name == "" || tg.DBPath == "" {
			t.Errorf("target has empty field: %+v", tg)
		}
		names[tg.Name] = true
	}
	// every app database is discovered, including the newer apps that the old
	// hardcoded list missed.
	for _, want := range []string{"content", "feed", "blobs", "daily", "bookmarks", "qr"} {
		if !names[want] {
			t.Errorf("targets() missing %q", want)
		}
	}
	// ...but never the backup app's own registry or a WAL sidecar.
	if names["backup"] {
		t.Error("targets() must not back up its own registry (backup.sqlite)")
	}
	if len(names) != 6 {
		t.Errorf("targets() picked up unexpected files: %v", names)
	}
}

// TestVerifySnapshot: a healthy SQLite file passes with its table count; a
// garbage file and an empty schema both fail.
func TestVerifySnapshot(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.sqlite")
	db, err := openDB(good)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	tables, err := verifySnapshot(good)
	if err != nil || tables == 0 {
		t.Fatalf("healthy snapshot: tables=%d err=%v", tables, err)
	}

	junk := filepath.Join(dir, "junk.sqlite")
	if err := os.WriteFile(junk, []byte("this is not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySnapshot(junk); err == nil {
		t.Fatal("garbage file passed verification")
	}
}

// TestSnapshotSkipsEmptyDatabases: a database with no tables records
// nothing — an empty file is not a restore point.
func TestSnapshotSkipsEmptyDatabases(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BACKUP_DB_PATH", filepath.Join(dir, "backup.sqlite"))
	reg, err := openDB(filepath.Join(dir, "backup.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	// A tables-free app database, the shape apex keeps for its sidecar.
	empty, err := store.OpenDB(filepath.Join(dir, "apex.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	empty.Close()

	results := snapshotAll(reg)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if r := results[0]; r.Err != "" || !r.Skipped {
		t.Fatalf("empty db result = %+v, want skipped with no error", r)
	}
	if n, _ := countBackups(reg); n != 0 {
		t.Fatalf("registry rows = %d, want 0", n)
	}
}
