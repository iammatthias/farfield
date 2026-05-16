package blob

import (
	"bytes"
	"crypto/rand"
	"os"
	"testing"

	"github.com/iammatthias/farfield/lib/core"
)

// TestR2RoundTrip exercises the R2 backend against a real bucket: Put, Exists,
// GetBytes, GetMeta, List, Delete. It is skipped unless the R2_* env vars are
// set, so the normal `go test ./...` run stays offline and credential-free.
//
//	export $(grep -v '^#' .env | xargs) && go test ./lib/blob -run TestR2RoundTrip -v
func TestR2RoundTrip(t *testing.T) {
	cfg := R2Config{
		AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:          os.Getenv("R2_BUCKET"),
	}
	if cfg.AccountID == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" || cfg.Bucket == "" {
		t.Skip("R2_* env vars unset — skipping live R2 round-trip")
	}

	r, err := NewR2(cfg)
	if err != nil {
		t.Fatalf("NewR2: %v", err)
	}

	// Random payload → a unique CID, so concurrent runs never collide.
	data := make([]byte, 256)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	meta := Meta{
		CID:           core.BlobCID(data),
		Size:          int64(len(data)),
		Mime:          "application/octet-stream",
		DominantColor: "#000000",
	}
	t.Logf("test blob CID: %s", meta.CID)

	// Clean up regardless of where the test fails.
	defer func() {
		if err := r.Delete(meta.CID); err != nil {
			t.Errorf("Delete (cleanup): %v", err)
		}
		if r.Exists(meta.CID) {
			t.Errorf("blob still Exists after Delete")
		}
	}()

	if err := r.Put(meta, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !r.Exists(meta.CID) {
		t.Fatalf("Exists is false right after Put")
	}

	got, err := r.GetBytes(meta.CID)
	if err != nil {
		t.Fatalf("GetBytes: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("GetBytes returned %d bytes, want %d", len(got), len(data))
	}

	gotMeta, err := r.GetMeta(meta.CID)
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if gotMeta == nil || gotMeta.CID != meta.CID || gotMeta.Size != meta.Size {
		t.Fatalf("GetMeta mismatch: got %+v, want %+v", gotMeta, meta)
	}

	cids, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, c := range cids {
		if c == meta.CID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("List (%d blobs) did not include the put CID", len(cids))
	}
}
