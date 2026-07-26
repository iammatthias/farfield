package backup

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteDBReplacesFileAndClearsSidecars(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "app.sqlite")

	if err := os.WriteFile(db, []byte("the old database"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A live SQLite database leaves these behind; a restored file must not
	// inherit the previous database's write-ahead log.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(db+suffix, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	want := []byte("the restored database")
	if err := WriteDB(db, bytes.NewReader(want)); err != nil {
		t.Fatalf("WriteDB: %v", err)
	}

	got, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("restored contents = %q, want %q", got, want)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(db + suffix); !os.IsNotExist(err) {
			t.Errorf("%s sidecar survived the restore", suffix)
		}
	}
	info, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %v, want 0644", perm)
	}
}

// The restore streams through a fixed buffer, so a database far larger than
// any sane heap allocation has to round-trip byte for byte.
func TestWriteDBRoundTripsLargeInput(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "big.sqlite")

	want := make([]byte, 8<<20)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	if err := WriteDB(db, bytes.NewReader(want)); err != nil {
		t.Fatalf("WriteDB: %v", err)
	}
	got, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("round trip changed %d bytes of input", len(want))
	}
}

// WriteDB stages into a sibling temp file and renames, so a read that fails
// partway must leave the previous database intact rather than truncated.
func TestWriteDBLeavesOriginalOnReadFailure(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "app.sqlite")
	original := []byte("the database that must survive")
	if err := os.WriteFile(db, original, 0o644); err != nil {
		t.Fatal(err)
	}

	err := WriteDB(db, &failingReader{after: 8})
	if err == nil {
		t.Fatal("WriteDB succeeded despite a failing source")
	}

	got, readErr := os.ReadFile(db)
	if readErr != nil {
		t.Fatalf("the original database is gone: %v", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("original was clobbered: %q", got)
	}

	// The staging file must not be left behind either.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".restore-") {
			t.Errorf("staging file %q survived the failure", e.Name())
		}
	}
}

func TestWriteDBCreatesFileWhenAbsent(t *testing.T) {
	db := filepath.Join(t.TempDir(), "new.sqlite")
	if err := WriteDB(db, strings.NewReader("fresh")); err != nil {
		t.Fatalf("WriteDB: %v", err)
	}
	got, err := os.ReadFile(db)
	if err != nil || string(got) != "fresh" {
		t.Errorf("contents = %q, err = %v", got, err)
	}
}

// failingReader yields `after` bytes and then errors, standing in for a
// connection that drops mid-download.
type failingReader struct{ after int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.after <= 0 {
		return 0, errTruncated
	}
	n := min(len(p), r.after)
	for i := range n {
		p[i] = 'x'
	}
	r.after -= n
	return n, nil
}

var errTruncated = &truncatedError{}

type truncatedError struct{}

func (*truncatedError) Error() string { return "connection dropped mid-transfer" }

// Pull streams the snapshot to an io.Writer rather than returning bytes, so a
// multi-hundred-MB restore costs the same memory as the backup that made it.
func TestPullStreamsToWriter(t *testing.T) {
	want := make([]byte, 3<<20)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		if r.URL.Path != "/backups/bafkreitest" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(want)
	}))
	defer srv.Close()

	var got bytes.Buffer
	if err := Pull(srv.URL, "secret-key", "bafkreitest", &got); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("pulled %d bytes, want %d — contents differ", got.Len(), len(want))
	}
	if gotKey != "secret-key" {
		t.Errorf("X-API-Key = %q, want the configured key", gotKey)
	}
}

func TestPullReportsUpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"blob not found"}`)
	}))
	defer srv.Close()

	var got bytes.Buffer
	err := Pull(srv.URL, "k", "bafkremissing", &got)
	if err == nil {
		t.Fatal("Pull succeeded against a 404")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "blob not found") {
		t.Errorf("error should carry the status and upstream message, got: %v", err)
	}
	if got.Len() != 0 {
		t.Errorf("error body was written to the destination (%d bytes)", got.Len())
	}
}

func TestPushFileStreamsAndReturnsCID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.sqlite")
	payload := bytes.Repeat([]byte("snapshot"), 4096)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	var gotLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"cid":"bafkreiuploaded"}`)
	}))
	defer srv.Close()

	cid, err := PushFile(srv.URL, "k", path)
	if err != nil {
		t.Fatalf("PushFile: %v", err)
	}
	if cid != "bafkreiuploaded" {
		t.Errorf("cid = %q, want bafkreiuploaded", cid)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Error("uploaded bytes differ from the file on disk")
	}
	// A declared length is what keeps the upload out of chunked encoding.
	if gotLen != int64(len(payload)) {
		t.Errorf("Content-Length = %d, want %d", gotLen, len(payload))
	}
}
