// Package store provides shared persistence helpers for farfield apps:
// environment loading, short-ID generation, and a sessions table with its
// CRUD helpers. It depends only on the standard library; the SQLite driver is
// imported by each app, not here.
package store

import (
	"bufio"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// idAlphabet is the character set used by ShortID — lowercase alphanumerics.
const idAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

// ShortID returns a 10-character random identifier drawn from idAlphabet. It
// is the farfield equivalent of nanoid: compact, URL-safe, and collision-safe
// for application record keys.
func ShortID() string {
	b := make([]byte, 10)
	rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}

// Env returns the value of the environment variable key, or def when the
// variable is unset or empty.
func Env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// LoadEnv finds the nearest .env file — the working directory or an ancestor
// of it — and loads any KEY=VALUE pairs not already set in the environment.
// In the farfield monorepo this resolves to the single .env at the repo root,
// whatever directory an app is started from. A missing .env is not an error.
func LoadEnv() error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	for {
		path := filepath.Join(dir, ".env")
		if _, err := os.Stat(path); err == nil {
			return loadEnvFile(path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil // reached the filesystem root — no .env
		}
		dir = parent
	}
}

// loadEnvFile parses a single .env file. Blank lines and lines beginning with
// '#' are ignored; surrounding double quotes are stripped from values; keys
// already present in the environment are left untouched.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// SessionSchema creates the sessions table. Apps using session auth should run
// it from their database-init routine alongside their own schema.
const SessionSchema = `
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	expires_at INTEGER NOT NULL
);`

// InsertSession stores a session token with its expiry time.
func InsertSession(db *sql.DB, token string, expiresAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO sessions (token, expires_at) VALUES (?, ?)`,
		token, expiresAt.Unix())
	return err
}

// ValidSession reports whether token names a session that exists and has not
// yet expired. An unknown token is reported as invalid, not as an error.
func ValidSession(db *sql.DB, token string) (bool, error) {
	var exp int64
	err := db.QueryRow(
		`SELECT expires_at FROM sessions WHERE token = ?`, token).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return time.Now().Unix() < exp, nil
}

// DeleteSession removes a session token, ending that session.
func DeleteSession(db *sql.DB, token string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// PruneSessions deletes every session whose expiry time has passed.
func PruneSessions(db *sql.DB) error {
	_, err := db.Exec(
		`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	return err
}
