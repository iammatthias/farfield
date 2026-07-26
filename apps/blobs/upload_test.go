package main

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"time"

	_ "modernc.org/sqlite"
)

// uploadServer wires just enough of the service to exercise the ingest path:
// a real database, a local byte store, and a spool directory.
func uploadServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "blobs.sqlite"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	bs, err := OpenLocalDir(filepath.Join(dir, "bytes"))
	if err != nil {
		t.Fatalf("OpenLocalDir: %v", err)
	}
	return &Server{
		db:        db,
		store:     bs,
		maxUpload: defaultMaxUpload,
		spoolDir:  filepath.Join(dir, "spool"),
	}
}

// Anything over inspectLimit takes the streaming path — spooled to disk,
// hashed in one pass, handed to the backend as a file. The result has to be
// indistinguishable from the in-memory path apart from the derived image
// fields, which large media does not have anyway.
func TestStoreUploadFromStreamsLargeInput(t *testing.T) {
	s := uploadServer(t)

	payload := make([]byte, inspectLimit+(1<<20))
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	// A QuickTime header: sniffMime refines this brand, and it must classify
	// identically from the 512-byte head the streaming path reads.
	copy(payload, append([]byte{0, 0, 0, 0x20}, []byte("ftypqt  ")...))

	m, err := s.storeUploadFrom(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("storeUploadFrom: %v", err)
	}
	if m.Size != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", m.Size, len(payload))
	}
	if m.CID != BlobCID(payload) {
		t.Errorf("CID = %q, want the content address of the input", m.CID)
	}
	if m.Mime != "video/quicktime" {
		t.Errorf("Mime = %q, want video/quicktime from the header alone", m.Mime)
	}

	stored, err := s.store.Get(m.CID)
	if err != nil {
		t.Fatalf("reading back stored bytes: %v", err)
	}
	if !bytes.Equal(stored, payload) {
		t.Error("stored bytes differ from the upload")
	}

	// The spool file must not outlive the request.
	entries, _ := os.ReadDir(s.spoolDir)
	if len(entries) != 0 {
		t.Errorf("spool directory still holds %d file(s)", len(entries))
	}
}

// Small uploads keep the in-memory path so images still get dimensions and a
// thumbnail — the reason the size split exists at all.
func TestStoreUploadFromInspectsSmallInput(t *testing.T) {
	s := uploadServer(t)
	png := onePixelPNG(t)

	m, err := s.storeUploadFrom(bytes.NewReader(png))
	if err != nil {
		t.Fatalf("storeUploadFrom: %v", err)
	}
	if m.Mime != "image/png" {
		t.Errorf("Mime = %q, want image/png", m.Mime)
	}
	if m.Width == 0 || m.Height == 0 {
		t.Errorf("small image did not get dimensions: %dx%d", m.Width, m.Height)
	}
}

func TestStoreUploadFromIsContentAddressed(t *testing.T) {
	s := uploadServer(t)
	payload := bytes.Repeat([]byte("dedupe me"), 1024)

	first, err := s.storeUploadFrom(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.storeUploadFrom(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if first.CID != second.CID {
		t.Errorf("identical bytes produced different CIDs: %s vs %s", first.CID, second.CID)
	}
	if first.CreatedAt != second.CreatedAt {
		t.Error("re-uploading known bytes created a new record instead of returning the existing one")
	}
}

func TestStoreUploadFromRejectsOversizeAndEmpty(t *testing.T) {
	s := uploadServer(t)
	s.maxUpload = 1 << 10

	// One byte past the cap must be refused, not silently truncated into a
	// corrupt blob under a valid-looking CID.
	over := bytes.Repeat([]byte("x"), int(s.maxUpload)+1)
	if _, err := s.storeUploadFrom(bytes.NewReader(over)); err == nil {
		t.Error("an oversize upload was accepted")
	}
	if _, err := s.storeUploadFrom(bytes.NewReader(nil)); err == nil {
		t.Error("an empty upload was accepted")
	}
	entries, _ := os.ReadDir(s.spoolDir)
	if len(entries) != 0 {
		t.Errorf("rejected uploads left %d spool file(s) behind", len(entries))
	}
}

// pruneSpool is the crash-recovery sweep: files a live request would have
// removed by defer, still present at startup.
func TestPruneSpoolRemovesStrandedFiles(t *testing.T) {
	s := uploadServer(t)
	if err := os.MkdirAll(s.spoolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(s.spoolDir, "upload-stranded")
	fresh := filepath.Join(s.spoolDir, "upload-inflight")
	other := filepath.Join(s.spoolDir, "not-an-upload")
	for _, p := range []string{stale, fresh, other} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Age the stranded one past the TTL.
	old := timeBeforeTTL()
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}

	s.pruneSpool()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stranded spool file survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("the sweep removed an in-flight upload")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("the sweep removed a file it does not own")
	}
}

func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	// A minimal valid 1x1 PNG, base64-free so the test stays readable.
	return []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
		0, 0, 0, 1, 0, 0, 0, 1, 8, 2, 0, 0, 0, 0x90, 0x77, 0x53, 0xde,
		0, 0, 0, 0x0c, 'I', 'D', 'A', 'T',
		0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00, 0x00, 0x03, 0x01, 0x01, 0x00,
		0x18, 0xdd, 0x8d, 0xb0,
		0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
	}
}

// timeBeforeTTL is a modification time old enough for the sweep to reclaim.
func timeBeforeTTL() time.Time {
	return time.Now().Add(-spoolTTL - time.Hour)
}
