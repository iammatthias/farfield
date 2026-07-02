// Package keys is the shared store for admin-issued, scoped API keys. The
// keys app mints and revokes them through one SQLite database (keys.sqlite on
// the shared /data volume); every keyed app opens the same file read-mostly
// and honors them alongside its env keys. Opaque random tokens — not JWTs —
// on purpose: everything runs on one host over one bind mount, so a database
// row gives instant revocation, per-key audit (created/last-used), and
// scoping with zero new dependencies, where a signed token would need TTLs,
// refresh flows, or a revocation list to approximate the same thing.
//
// Only the token's SHA-256 lands on disk; the plaintext exists once, in the
// response to whoever minted it. Like lib/store, this package stays
// standard-library only — the calling module imports the SQLite driver.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
)

// Scopes, narrowest to broadest. write implies the others; upload and read
// are siblings (an upload key cannot read, a read key cannot upload).
const (
	ScopeRead   = "read"
	ScopeUpload = "upload"
	ScopeWrite  = "write"
)

// AppAny is the wildcard app value: the key works on every app.
const AppAny = "*"

// tokenPrefix marks farfield-minted keys so they are recognizable in configs
// and secret scanners.
const tokenPrefix = "ffk_"

const schema = `
CREATE TABLE IF NOT EXISTS api_keys (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	app          TEXT NOT NULL,
	scope        TEXT NOT NULL,
	hash         TEXT NOT NULL UNIQUE,
	hint         TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	expires_at   TEXT,
	revoked_at   TEXT,
	last_used_at TEXT
);`

// Key is one issued key. The token itself is never stored — Hash is its
// SHA-256 and Hint its first characters, enough to match a key against a
// config by eye.
type Key struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	App       string `json:"app"`
	Scope     string `json:"scope"`
	Hint      string `json:"hint"`
	CreatedAt string `json:"createdAt"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	RevokedAt string `json:"revokedAt,omitempty"`
	LastUsed  string `json:"lastUsedAt,omitempty"`
}

// Active reports whether the key is usable right now.
func (k *Key) Active() bool {
	if k.RevokedAt != "" {
		return false
	}
	if k.ExpiresAt != "" && k.ExpiresAt <= store.NowRFC3339() {
		return false
	}
	return true
}

// Store hands out and checks keys against one SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the key database at path. The calling
// module must import the SQLite driver.
func Open(path string) (*Store, error) {
	db, err := store.OpenDB(path)
	if err != nil {
		return nil, err
	}
	s, err := New(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// New wraps an already-open database, ensuring the key schema — for the keys
// app itself, which shares one connection between keys and its sessions.
func New(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// ValidScope reports whether scope is one of the known scopes.
func ValidScope(scope string) bool {
	return scope == ScopeRead || scope == ScopeUpload || scope == ScopeWrite
}

// Mint creates a key for app with scope, returning the plaintext token —
// shown exactly once — and the stored record. A zero expires means the key
// never expires.
func (s *Store) Mint(name, app, scope string, expires time.Time) (string, *Key, error) {
	name = strings.TrimSpace(name)
	app = strings.TrimSpace(app)
	if name == "" {
		return "", nil, errors.New("key name is required")
	}
	if app == "" {
		return "", nil, errors.New("app is required")
	}
	if !ValidScope(scope) {
		return "", nil, fmt.Errorf("unknown scope %q", scope)
	}
	token := tokenPrefix + strings.ToLower(rand.Text()) + strings.ToLower(rand.Text())
	k := &Key{
		ID:        store.ShortID(),
		Name:      name,
		App:       app,
		Scope:     scope,
		Hint:      token[:len(tokenPrefix)+6],
		CreatedAt: store.NowRFC3339(),
	}
	if !expires.IsZero() {
		k.ExpiresAt = expires.UTC().Format(time.RFC3339)
	}
	_, err := s.db.Exec(`INSERT INTO api_keys
		(id, name, app, scope, hash, hint, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.App, k.Scope, hashToken(token), k.Hint,
		k.CreatedAt, nullable(k.ExpiresAt))
	if err != nil {
		return "", nil, err
	}
	return token, k, nil
}

// Check resolves a presented token for app: it returns the key's scope when
// the token names an active key issued for that app (or for every app).
// Lookup is by SHA-256, so timing reveals nothing about stored tokens. A hit
// stamps last_used_at best-effort — an audit hint, never a gate.
func (s *Store) Check(token, app string) (string, bool) {
	if token == "" || !strings.HasPrefix(token, tokenPrefix) {
		return "", false
	}
	var k Key
	var expires, revoked sql.NullString
	err := s.db.QueryRow(`SELECT id, app, scope, expires_at, revoked_at
		FROM api_keys WHERE hash = ?`, hashToken(token)).
		Scan(&k.ID, &k.App, &k.Scope, &expires, &revoked)
	if err != nil {
		return "", false
	}
	k.ExpiresAt, k.RevokedAt = expires.String, revoked.String
	if !k.Active() || (k.App != AppAny && k.App != app) {
		return "", false
	}
	_, _ = s.db.Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`,
		store.NowRFC3339(), k.ID)
	return k.Scope, true
}

// Revoke deactivates a key immediately. Revoking an already-revoked key is a
// no-op; an unknown id reports false.
func (s *Store) Revoke(id string) (bool, error) {
	res, err := s.db.Exec(`UPDATE api_keys SET revoked_at = ?
		WHERE id = ? AND revoked_at IS NULL`, store.NowRFC3339(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Delete removes a key record entirely — for tidying long-revoked keys; use
// Revoke to deactivate.
func (s *Store) Delete(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// List returns every key, newest first.
func (s *Store) List() ([]Key, error) {
	rows, err := s.db.Query(`SELECT id, name, app, scope, hint, created_at,
		expires_at, revoked_at, last_used_at
		FROM api_keys ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		var expires, revoked, used sql.NullString
		if err := rows.Scan(&k.ID, &k.Name, &k.App, &k.Scope, &k.Hint,
			&k.CreatedAt, &expires, &revoked, &used); err != nil {
			return nil, err
		}
		k.ExpiresAt, k.RevokedAt, k.LastUsed = expires.String, revoked.String, used.String
		out = append(out, k)
	}
	return out, rows.Err()
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// nullable maps "" to NULL so optional timestamps stay NULL, not empty text.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
