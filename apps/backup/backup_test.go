package main

import (
	"os"
	"path/filepath"
	"testing"
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
