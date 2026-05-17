// Package backup snapshots and restores farfield SQLite databases and moves
// the snapshots to and from the blobs service, which stores them in R2. A
// snapshot is the whole database — every markdown body included — so it is a
// complete, restorable backup. Standard library only.
package backup

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CID returns the content identifier of data — a CIDv1 (raw codec, sha2-256,
// multibase-base32), the same scheme the blobs service uses. Identical bytes
// always yield the same CID, so an unchanged database hashes to a CID already
// on record — which is how the backup app skips needless uploads.
func CID(data []byte) string {
	digest := sha256.Sum256(data)
	buf := make([]byte, 0, 4+sha256.Size)
	buf = append(buf, 0x01, 0x55, 0x12, 0x20)
	buf = append(buf, digest[:]...)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
	return "b" + strings.ToLower(enc)
}

// Snapshot returns a consistent point-in-time copy of db's contents, produced
// by SQLite's `VACUUM INTO`. It is safe to call against a live database.
func Snapshot(db *sql.DB) ([]byte, error) {
	var r [8]byte
	_, _ = rand.Read(r[:])
	tmp := filepath.Join(os.TempDir(), "farfield-snapshot-"+hex.EncodeToString(r[:])+".sqlite")
	// VACUUM INTO's target must not exist; the random name guarantees that.
	stmt := "VACUUM INTO '" + strings.ReplaceAll(tmp, "'", "''") + "'"
	if _, err := db.Exec(stmt); err != nil {
		return nil, fmt.Errorf("VACUUM INTO: %w", err)
	}
	defer os.Remove(tmp)
	return os.ReadFile(tmp)
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

// Push uploads a snapshot to the blobs service's /backups endpoint and returns
// its content-addressed CID.
func Push(blobsURL, apiKey string, data []byte) (string, error) {
	req, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(blobsURL, "/")+"/backups", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
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
