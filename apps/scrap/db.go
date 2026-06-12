package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Visibility levels, most to least exposed.
const (
	VisPublic   = "public"   // listed on the public index
	VisUnlisted = "unlisted" // link-only
	VisPrivate  = "private"  // author session required
)

func validVisibility(v string) bool {
	return v == VisPublic || v == VisUnlisted || v == VisPrivate
}

// Paste is one stored paste. The ID is the short content address —
// cid.Of(body)[:16] — so identical bodies collapse to one row; CID keeps the
// full content identifier. Alias is reserved for P1 named/editable pastes:
// the column ships now (with its partial unique index) but no P0 UI sets it.
type Paste struct {
	ID         string `json:"id"`
	CID        string `json:"cid"`
	Title      string `json:"title"`
	Lang       string `json:"lang"`
	Body       string `json:"body"`
	Visibility string `json:"visibility"`
	ExpiresAt  string `json:"expiresAt"` // RFC 3339 UTC, '' = never
	CreatedAt  string `json:"createdAt"`
	Views      int64  `json:"views"`
	Alias      string `json:"alias,omitempty"`
	HasToken   bool   `json:"hasToken"`
}

// Size returns the body length in bytes.
func (p *Paste) Size() int { return len(p.Body) }

// schema — pastes plus their view tokens. The alias UNIQUE constraint is a
// partial index (” would collide as a plain UNIQUE column); tokens are a set
// per paste, capped at one in P0 by setToken, so per-recipient tokens in P1
// lift the cap without a schema change.
const schema = `
CREATE TABLE IF NOT EXISTS pastes (
	id         TEXT PRIMARY KEY,
	cid        TEXT NOT NULL DEFAULT '',
	title      TEXT NOT NULL DEFAULT '',
	lang       TEXT NOT NULL DEFAULT '',
	body       TEXT NOT NULL DEFAULT '',
	visibility TEXT NOT NULL DEFAULT 'unlisted',
	expires_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	views      INTEGER NOT NULL DEFAULT 0,
	alias      TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS pastes_alias
	ON pastes (alias) WHERE alias != '';
CREATE INDEX IF NOT EXISTS pastes_by_visibility
	ON pastes (visibility, created_at DESC);
CREATE INDEX IF NOT EXISTS pastes_by_expiry
	ON pastes (expires_at) WHERE expires_at != '';

CREATE TABLE IF NOT EXISTS tokens (
	paste_id   TEXT NOT NULL,
	name       TEXT NOT NULL DEFAULT '',
	hash       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	last_used  TEXT NOT NULL DEFAULT '',
	uses       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (paste_id, hash)
);`

// openDB opens the SQLite database, applies pragmas, runs the schema, and
// performs idempotent column-add migrations — every step is safe to run on
// every startup (see the self-migrating-sqlite skill).
func openDB(path string) (*sql.DB, error) {
	db, err := store.OpenDB(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if _, err := db.Exec(store.SessionSchema); err != nil {
		return nil, err
	}
	for _, c := range []struct{ col, decl string }{
		{"cid", "TEXT NOT NULL DEFAULT ''"},
		{"title", "TEXT NOT NULL DEFAULT ''"},
		{"lang", "TEXT NOT NULL DEFAULT ''"},
		{"visibility", "TEXT NOT NULL DEFAULT 'unlisted'"},
		{"expires_at", "TEXT NOT NULL DEFAULT ''"},
		{"views", "INTEGER NOT NULL DEFAULT 0"},
		{"alias", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := store.EnsureColumn(db, "pastes", c.col, c.decl); err != nil {
			return nil, err
		}
	}
	return db, nil
}

// pasteID derives the content address of a body: the full CIDv1 and the
// 16-character short form used as the primary key and in URLs.
func pasteID(body string) (short, full string) {
	full = cid.Of([]byte(body))
	return full[:16], full
}

// pasteCols selects a full row plus the computed has_token flag.
const pasteCols = `p.id, p.cid, p.title, p.lang, p.body, p.visibility,
	p.expires_at, p.created_at, p.views, p.alias,
	EXISTS (SELECT 1 FROM tokens t WHERE t.paste_id = p.id)`

type scanner interface{ Scan(...any) error }

func scanPaste(row scanner) (*Paste, error) {
	var p Paste
	var hasToken int
	if err := row.Scan(&p.ID, &p.CID, &p.Title, &p.Lang, &p.Body,
		&p.Visibility, &p.ExpiresAt, &p.CreatedAt, &p.Views, &p.Alias,
		&hasToken); err != nil {
		return nil, err
	}
	p.HasToken = hasToken != 0
	return &p, nil
}

// upsertPaste writes a paste under its content address. Identical bodies
// collapse to one row: a re-create updates the metadata (title, lang,
// visibility, expiry) but keeps the original created_at, views, and alias.
func upsertPaste(db *sql.DB, p *Paste) error {
	p.CreatedAt = store.NowRFC3339()
	_, err := db.Exec(`INSERT INTO pastes
		(id, cid, title, lang, body, visibility, expires_at, created_at, views, alias)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, '')
		ON CONFLICT(id) DO UPDATE SET
			title      = excluded.title,
			lang       = excluded.lang,
			visibility = excluded.visibility,
			expires_at = excluded.expires_at`,
		p.ID, p.CID, p.Title, p.Lang, p.Body, p.Visibility, p.ExpiresAt,
		p.CreatedAt)
	return err
}

// getPaste returns a paste by short id, or (nil, nil) when absent.
func getPaste(db *sql.DB, id string) (*Paste, error) {
	p, err := scanPaste(db.QueryRow(
		`SELECT `+pasteCols+` FROM pastes p WHERE p.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func collectPastes(rows *sql.Rows) ([]Paste, error) {
	defer rows.Close()
	out := []Paste{}
	for rows.Next() {
		p, err := scanPaste(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// listPublicPastes returns public, unexpired pastes, newest first — the rows
// the anonymous /pastes index shows.
func listPublicPastes(db *sql.DB) ([]Paste, error) {
	rows, err := db.Query(`SELECT `+pasteCols+` FROM pastes p
		WHERE p.visibility = ? AND (p.expires_at = '' OR p.expires_at > ?)
		ORDER BY p.created_at DESC, p.id`, VisPublic, store.NowRFC3339())
	if err != nil {
		return nil, err
	}
	return collectPastes(rows)
}

// listManagePastes returns every paste, filtered for the manage table.
// visibility/lang filter exactly when non-empty; q substring-matches title
// and body.
func listManagePastes(db *sql.DB, visibility, lang, q string) ([]Paste, error) {
	where := []string{"1=1"}
	args := []any{}
	if visibility != "" {
		where = append(where, "p.visibility = ?")
		args = append(args, visibility)
	}
	if lang != "" {
		where = append(where, "p.lang = ?")
		args = append(args, lang)
	}
	if q != "" {
		where = append(where, "(p.title LIKE ? ESCAPE '\\' OR p.body LIKE ? ESCAPE '\\')")
		pat := "%" + escapeLike(q) + "%"
		args = append(args, pat, pat)
	}
	rows, err := db.Query(`SELECT `+pasteCols+` FROM pastes p
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY p.created_at DESC, p.id`, args...)
	if err != nil {
		return nil, err
	}
	return collectPastes(rows)
}

// escapeLike escapes LIKE metacharacters so a search for "100%" matches
// literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// distinctLangs returns the languages in use, for the manage filter select.
func distinctLangs(db *sql.DB) ([]string, error) {
	rows, err := db.Query(
		`SELECT DISTINCT lang FROM pastes WHERE lang != '' ORDER BY lang`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// countPastes returns the total number of stored pastes — the /status figure.
func countPastes(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pastes`).Scan(&n)
	return n, err
}

// incrementViews bumps a paste's view counter.
func incrementViews(db *sql.DB, id string) error {
	_, err := db.Exec(`UPDATE pastes SET views = views + 1 WHERE id = ?`, id)
	return err
}

// deletePaste removes a paste and its tokens.
func deletePaste(db *sql.DB, id string) (bool, error) {
	if _, err := db.Exec(`DELETE FROM tokens WHERE paste_id = ?`, id); err != nil {
		return false, err
	}
	res, err := db.Exec(`DELETE FROM pastes WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// deleteExpiredPastes removes every paste whose expiry has passed, with its
// tokens. RFC 3339 UTC strings compare lexically, so <= is a time comparison.
func deleteExpiredPastes(db *sql.DB) (int64, error) {
	now := store.NowRFC3339()
	if _, err := db.Exec(`DELETE FROM tokens WHERE paste_id IN
		(SELECT id FROM pastes WHERE expires_at != '' AND expires_at <= ?)`,
		now); err != nil {
		return 0, err
	}
	res, err := db.Exec(
		`DELETE FROM pastes WHERE expires_at != '' AND expires_at <= ?`, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// expired reports whether the paste's expiry has passed.
func expired(p *Paste) bool {
	return p.ExpiresAt != "" && p.ExpiresAt <= store.NowRFC3339()
}

// ── view tokens ────────────────────────────────────────────────────────────

// tokenHash is how token secrets are stored: hex sha-256, never plaintext.
func tokenHash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// setToken replaces a paste's view token. P0 caps the token set at one —
// replace-on-set — so P1 per-recipient tokens can lift the cap by switching
// to a plain insert.
func setToken(db *sql.DB, pasteID, secret, name string) error {
	if _, err := db.Exec(`DELETE FROM tokens WHERE paste_id = ?`, pasteID); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT INTO tokens (paste_id, name, hash, created_at)
		VALUES (?, ?, ?, ?)`, pasteID, name, tokenHash(secret), store.NowRFC3339())
	return err
}

// deleteTokens removes every token row for a paste — the paste serves per its
// plain visibility again. Reports whether any token existed.
func deleteTokens(db *sql.DB, pasteID string) (bool, error) {
	res, err := db.Exec(`DELETE FROM tokens WHERE paste_id = ?`, pasteID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// setVisibility updates a paste's visibility in place.
func setVisibility(db *sql.DB, id, visibility string) error {
	_, err := db.Exec(`UPDATE pastes SET visibility = ? WHERE id = ?`,
		visibility, id)
	return err
}

// verifyToken checks a presented secret against the paste's stored hashes in
// constant time. A match updates last_used and the use counter.
func verifyToken(db *sql.DB, pasteID, secret string) (bool, error) {
	rows, err := db.Query(`SELECT hash FROM tokens WHERE paste_id = ?`, pasteID)
	if err != nil {
		return false, err
	}
	got := tokenHash(secret)
	matched := ""
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return false, err
		}
		// Compare every candidate — no early exit — so timing does not reveal
		// which (or whether any) hash matched.
		if subtle.ConstantTimeCompare([]byte(got), []byte(h)) == 1 {
			matched = h
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if matched == "" {
		return false, nil
	}
	_, err = db.Exec(`UPDATE tokens SET last_used = ?, uses = uses + 1
		WHERE paste_id = ? AND hash = ?`, store.NowRFC3339(), pasteID, matched)
	return true, err
}

// ── expiry parsing ─────────────────────────────────────────────────────────

// expiryChoices are the compose/API expiry options, in display order.
var expiryChoices = []string{"never", "1h", "1d", "1w", "1m"}

// parseExpiry maps an expiry choice to an absolute RFC 3339 deadline (” for
// never). Unknown values are an error so a typo never silently means forever.
func parseExpiry(choice string) (string, error) {
	var d time.Duration
	switch choice {
	case "", "never":
		return "", nil
	case "1h":
		d = time.Hour
	case "1d":
		d = 24 * time.Hour
	case "1w":
		d = 7 * 24 * time.Hour
	case "1m":
		d = 30 * 24 * time.Hour
	default:
		return "", fmt.Errorf("unknown expiry %q", choice)
	}
	return time.Now().UTC().Add(d).Format(time.RFC3339), nil
}
