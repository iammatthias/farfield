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
	"log/slog"
	"strings"
	"sync"
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
);` + usageSchema

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

// stampInterval bounds how often a key's last_used_at is rewritten. Check
// runs on every authenticated request across the whole fleet, and each app
// opens the same keys.sqlite over one bind mount — stamping every hit turned
// a read path into a fleet-wide write path contending for the same file lock.
// last_used_at is an audit hint, never a gate, so five-minute resolution is
// all it ever needed.
const stampInterval = 5 * time.Minute

// Store hands out and checks keys against one SQLite database.
type Store struct {
	db *sql.DB

	// stampMu guards stamped, the in-process record of when each key's
	// last_used_at was last written.
	stampMu sync.Mutex
	stamped map[string]time.Time

	// usageMu guards usage, the in-process rollups waiting for the next
	// flush (see usage.go). stop ends the flusher; closeOnce makes Close
	// safe to call twice.
	usageMu   sync.Mutex
	usage     map[usageKey]*usageAgg
	stop      chan struct{}
	flushed   chan struct{}
	closeOnce sync.Once
}

// shouldStamp reports whether this key's last_used_at is due for a rewrite,
// recording the decision. Per-process: several app containers may each stamp
// once per interval, which is still a rounding error against once per request.
func (s *Store) shouldStamp(id string, now time.Time) bool {
	s.stampMu.Lock()
	defer s.stampMu.Unlock()
	if last, ok := s.stamped[id]; ok && now.Sub(last) < stampInterval {
		return false
	}
	s.stamped[id] = now
	return true
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
	s := &Store{
		db:      db,
		stamped: make(map[string]time.Time),
		usage:   make(map[usageKey]*usageAgg),
		stop:    make(chan struct{}),
		flushed: make(chan struct{}),
	}
	go s.flushLoop()
	return s, nil
}

// Close stops the usage flusher, writes the rollups still in memory, and
// releases the underlying database. It is safe to call more than once.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.flushed
		if ferr := s.Flush(); ferr != nil {
			slog.Warn("keys: final usage flush failed", "err", ferr)
		}
		err = s.db.Close()
	})
	return err
}

// ValidScope reports whether scope is one of the known scopes.
func ValidScope(scope string) bool {
	return scope == ScopeRead || scope == ScopeUpload || scope == ScopeWrite
}

// Mint creates a key for app with scope, returning the plaintext token —
// shown exactly once — and the stored record. A zero expires means the key
// never expires.
func (s *Store) Mint(name, app, scope string, expires time.Time) (string, *Key, error) {
	return mint(s.db, name, app, scope, expires)
}

// execer is the slice of *sql.DB and *sql.Tx that mint needs, so Rotate can
// mint inside its transaction.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func mint(db execer, name, app, scope string, expires time.Time) (string, *Key, error) {
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
	_, err := db.Exec(`INSERT INTO api_keys
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
// stamps last_used_at best-effort, at most once per stampInterval — an audit
// hint, never a gate. Check counts toward the key's usage with no route; the
// fleet's gates call CheckRequest instead.
func (s *Store) Check(token, app string) (string, bool) {
	return s.CheckRequest(token, app, "", "")
}

// CheckRequest is Check plus the request's method and route, for the usage
// rollups: a token that names a key is counted in memory — accepted, or
// refused because the key is revoked, expired, or issued for another app —
// and flushed in batches (see usage.go). A token that names no key is not
// recorded at all. lib/web's gates find this method through an optional
// interface, so lib/web never imports this package.
func (s *Store) CheckRequest(token, app, method, path string) (string, bool) {
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
	now := time.Now()
	switch {
	case k.RevokedAt != "":
		s.record(k.ID, app, method, path, OutcomeRevoked, now)
		return "", false
	case !k.Active():
		s.record(k.ID, app, method, path, OutcomeExpired, now)
		return "", false
	case k.App != AppAny && k.App != app:
		s.record(k.ID, app, method, path, OutcomeWrongApp, now)
		return "", false
	}
	s.record(k.ID, app, method, path, OutcomeOK, now)
	if s.shouldStamp(k.ID, now) {
		_, _ = s.db.Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`,
			now.UTC().Format(time.RFC3339), k.ID)
	}
	return k.Scope, true
}

// Get returns one key by id, or nil when there is no such key.
func (s *Store) Get(id string) (*Key, error) {
	var k Key
	var expires, revoked, used sql.NullString
	err := s.db.QueryRow(`SELECT id, name, app, scope, hint, created_at,
		expires_at, revoked_at, last_used_at FROM api_keys WHERE id = ?`, id).
		Scan(&k.ID, &k.Name, &k.App, &k.Scope, &k.Hint,
			&k.CreatedAt, &expires, &revoked, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k.ExpiresAt, k.RevokedAt, k.LastUsed = expires.String, revoked.String, used.String
	return &k, nil
}

// Rename changes a key's display name; an unknown id reports false.
func (s *Store) Rename(id, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errors.New("key name is required")
	}
	res, err := s.db.Exec(`UPDATE api_keys SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetExpiry sets when a key stops working; a zero time clears the expiry so
// the key never expires. Extending an expired key revives it; a revoked key
// stays revoked whatever its expiry. An unknown id reports false.
func (s *Store) SetExpiry(id string, expires time.Time) (bool, error) {
	var v any
	if !expires.IsZero() {
		v = expires.UTC().Format(time.RFC3339)
	}
	res, err := s.db.Exec(`UPDATE api_keys SET expires_at = ? WHERE id = ?`, v, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ErrNotActive is returned by Rotate for a key that is revoked or expired —
// there is nothing live to replace; extend or reissue it instead.
var ErrNotActive = errors.New("only an active key can be rotated")

// Rotate replaces an active key: in one transaction it mints a new key with
// the same name, app, scope and expiry, and revokes the old one. It returns
// the new plaintext token — shown exactly once, as from Mint — and the new
// record. An unknown id returns a nil key and no error.
func (s *Store) Rotate(id string) (string, *Key, error) {
	old, err := s.Get(id)
	if err != nil || old == nil {
		return "", nil, err
	}
	if !old.Active() {
		return "", nil, ErrNotActive
	}
	var expires time.Time
	if old.ExpiresAt != "" {
		if expires, err = time.Parse(time.RFC3339, old.ExpiresAt); err != nil {
			return "", nil, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()
	token, k, err := mint(tx, old.Name, old.App, old.Scope, expires)
	if err != nil {
		return "", nil, err
	}
	res, err := tx.Exec(`UPDATE api_keys SET revoked_at = ?
		WHERE id = ? AND revoked_at IS NULL`, store.NowRFC3339(), id)
	if err != nil {
		return "", nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", nil, ErrNotActive // revoked between the read and the write
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return token, k, nil
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

// Delete removes a key record entirely, with its usage rollups — for tidying
// long-revoked keys; use Revoke to deactivate.
func (s *Store) Delete(id string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM key_usage WHERE key_id = ?`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
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
