package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// restoreFixture builds a data directory holding the backup registry and one
// app database, plus a stand-in blobs service. It returns the directory and
// the server so a test can drive runRestore end to end.
func restoreFixture(t *testing.T, snapshot []byte) (dir string, blobs *httptest.Server) {
	t.Helper()
	dir = t.TempDir()

	// The backup app's own registry.
	registry := filepath.Join(dir, "backup.sqlite")
	db, err := openDB(registry)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	db.Close()

	// A target app database with recognisable contents.
	appDB, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "content.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appDB.Exec(`CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('live')`); err != nil {
		t.Fatal(err)
	}
	appDB.Close()

	blobs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/backups":
			// The pre-restore safety snapshot lands here.
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"cid":"bafkreisafety"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/backups/"):
			if snapshot == nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(snapshot)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(blobs.Close)

	t.Setenv("BACKUP_DB_PATH", registry)
	t.Setenv("BLOBS_URL", blobs.URL)
	t.Setenv("BLOBS_API_KEY", "test-key")
	return dir, blobs
}

// A dry run is the default for a reason: it must report what it would do and
// leave the target database exactly as it found it.
func TestRunRestoreDryRunDoesNotWrite(t *testing.T) {
	dir, _ := restoreFixture(t, []byte("a replacement database"))
	target := filepath.Join(dir, "content.sqlite")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	if err := runRestore("content", "bafkreisnapshot", false); err != nil {
		t.Fatalf("dry run returned an error: %v", err)
	}

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("a dry run modified the target database")
	}
	// The staged download must not be left in the data directory either.
	assertNoStagingLeftovers(t, dir)
}

func TestRunRestoreWithConfirmReplacesTarget(t *testing.T) {
	want := "a replacement database"
	dir, _ := restoreFixture(t, []byte(want))
	target := filepath.Join(dir, "content.sqlite")

	// Sidecars from the running service must not survive the swap.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(target+suffix, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := runRestore("content", "bafkreisnapshot", true); err != nil {
		t.Fatalf("runRestore: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("target contents = %q, want %q", got, want)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(target + suffix); !os.IsNotExist(err) {
			t.Errorf("%s sidecar survived the restore", suffix)
		}
	}
	assertNoStagingLeftovers(t, dir)
}

// An unknown app must fail before anything is downloaded or written, and say
// which apps it did find.
func TestRunRestoreUnknownApp(t *testing.T) {
	restoreFixture(t, []byte("unused"))
	err := runRestore("nosuchapp", "bafkreisnapshot", true)
	if err == nil {
		t.Fatal("restoring an unknown app succeeded")
	}
	if !strings.Contains(err.Error(), "nosuchapp") {
		t.Errorf("error should name the unknown app, got: %v", err)
	}
	if !strings.Contains(err.Error(), "content") {
		t.Errorf("error should list the available apps, got: %v", err)
	}
}

// A snapshot the blobs service cannot supply must leave the target intact —
// the failure has to happen before the swap, not halfway through it.
func TestRunRestoreMissingSnapshotLeavesTargetIntact(t *testing.T) {
	dir, _ := restoreFixture(t, nil) // every GET 404s
	target := filepath.Join(dir, "content.sqlite")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	if err := runRestore("content", "bafkreimissing", true); err == nil {
		t.Fatal("restore succeeded despite an unavailable snapshot")
	}

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the target database is gone: %v", err)
	}
	if string(before) != string(after) {
		t.Error("the target was modified despite a failed download")
	}
	assertNoStagingLeftovers(t, dir)
}

func assertNoStagingLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "restore-") || strings.Contains(e.Name(), ".restore-") {
			t.Errorf("staging file %q was left behind", e.Name())
		}
	}
}
