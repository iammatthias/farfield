// Package backup snapshots and restores farfield SQLite databases and moves
// the snapshots to and from the blobs service, which stores them in R2. A
// snapshot is the whole database — every markdown body included — so it is a
// complete, restorable backup. Standard library only.
package backup

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
)

// Snapshot writes a consistent point-in-time copy of db's contents to a temp
// file via SQLite's `VACUUM INTO` and returns the file's path. It is safe to
// call against a live database. The caller removes the file when done — the
// snapshot stays on disk, never in memory, so multi-hundred-MB databases
// back up in constant space.
func Snapshot(db *sql.DB) (string, error) {
	var r [8]byte
	_, _ = rand.Read(r[:])
	tmp := filepath.Join(os.TempDir(), "farfield-snapshot-"+hex.EncodeToString(r[:])+".sqlite")
	// VACUUM INTO's target must not exist; the random name guarantees that.
	stmt := "VACUUM INTO '" + strings.ReplaceAll(tmp, "'", "''") + "'"
	if _, err := db.Exec(stmt); err != nil {
		return "", fmt.Errorf("VACUUM INTO: %w", err)
	}
	return tmp, nil
}

// FileCID streams the file at path once and returns its CIDv1 — the same
// scheme the blobs service uses, so an unchanged database hashes to a CID
// already on record — along with its size in bytes.
func FileCID(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return cid.OfReader(f)
}

// WriteDB replaces the database file at path with the bytes from src,
// clearing any stale -wal/-shm sidecars. The copy streams through a fixed
// buffer, so restoring a multi-hundred-MB database costs no more memory than
// taking the snapshot did. The owning service must not be running when this
// is called, or it will keep operating on the pre-restore file.
//
// The bytes land in a sibling temp file and are renamed into place, so an
// interrupted restore leaves the previous database intact rather than a
// half-written one.
func WriteDB(path string, src io.Reader) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".restore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	// fsync before the rename: a crash must not leave the new name pointing
	// at bytes the kernel has not committed.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	return nil
}

var client = &http.Client{Timeout: 5 * time.Minute}

// PushFile streams the snapshot file at path to the blobs service's /backups
// endpoint and returns its content-addressed CID. The body is the open file —
// the snapshot is never buffered in memory.
func PushFile(blobsURL, apiKey, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(blobsURL, "/")+"/backups", f)
	if err != nil {
		return "", err
	}
	req.ContentLength = info.Size()
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Content-Type", "application/x-sqlite3")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// Both the success body ({"cid":...}) and any error body are small;
	// bound the read so an upstream that streams forever cannot be believed.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("blobs /backups: HTTP %d: %s", resp.StatusCode, body)
	}
	var out struct {
		CID string `json:"cid"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	return out.CID, nil
}

// maxErrorBody caps how much of a failed response we read back for the error
// message. Enough for any JSON error the blobs service returns; bounded so a
// misbehaving upstream cannot make an error path allocate without limit.
const maxErrorBody = 8 << 10

// Pull streams a snapshot by CID from the blobs service into dst. It is the
// counterpart to PushFile: the snapshot passes through in fixed-size chunks
// and is never held in memory, so restore is as constant-space as backup.
func Pull(blobsURL, apiKey, cid string, dst io.Writer) error {
	req, err := http.NewRequest(http.MethodGet,
		strings.TrimRight(blobsURL, "/")+"/backups/"+cid, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("blobs /backups/%s: HTTP %d: %s", cid, resp.StatusCode, body)
	}
	_, err = io.Copy(dst, resp.Body)
	return err
}

// Delete removes a snapshot by CID from the blobs service.
func Delete(blobsURL, apiKey, cid string) error {
	req, err := http.NewRequest(http.MethodDelete,
		strings.TrimRight(blobsURL, "/")+"/backups/"+cid, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("blobs DELETE /backups/%s: HTTP %d: %s", cid, resp.StatusCode, body)
	}
	return nil
}
