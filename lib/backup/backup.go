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

// WriteDB replaces the database file at path with data, clearing any stale
// -wal/-shm sidecars. The owning service must not be running when this is
// called, or it will keep operating on the pre-restore file.
func WriteDB(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o644); err != nil {
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
	body, _ := io.ReadAll(resp.Body)
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

// Pull downloads a snapshot by CID from the blobs service.
func Pull(blobsURL, apiKey, cid string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet,
		strings.TrimRight(blobsURL, "/")+"/backups/"+cid, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("blobs /backups/%s: HTTP %d: %s", cid, resp.StatusCode, body)
	}
	return body, nil
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
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("blobs DELETE /backups/%s: HTTP %d: %s", cid, resp.StatusCode, body)
	}
	return nil
}
