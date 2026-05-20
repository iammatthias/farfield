package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Mode is how a Code's QR is encoded.
//
//   - direct  — the QR encodes Target verbatim, immutably from the admin's view
//   - proxy   — the QR encodes a stable public URL on this service that
//     redirects to the current Target; editing Target later does not
//     change the QR.
type Mode string

const (
	ModeDirect Mode = "direct"
	ModeProxy  Mode = "proxy"
)

func validMode(m Mode) bool { return m == ModeDirect || m == ModeProxy }

// Code is one stored QR record. The ID is the stable short-key both the admin
// UI and proxy redirects reference; CID is a content hash that moves when
// visible content/config changes (Mode, Target, EC, Enabled, Public, Label).
// AdminNotes and timestamps are excluded from the CID so admin-only edits and
// metadata churn do not invalidate caches.
type Code struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Mode       Mode   `json:"mode"`
	Target     string `json:"target"`
	EC         string `json:"ec"` // "L"/"M"/"Q"/"H"
	Public     bool   `json:"public"`
	Enabled    bool   `json:"enabled"`
	AdminNotes string `json:"adminNotes,omitempty"`
	CID        string `json:"cid"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

const schema = `
CREATE TABLE IF NOT EXISTS codes (
	id           TEXT PRIMARY KEY,
	label        TEXT NOT NULL DEFAULT '',
	mode         TEXT NOT NULL DEFAULT 'direct',
	target       TEXT NOT NULL DEFAULT '',
	ec           TEXT NOT NULL DEFAULT 'M',
	public       INTEGER NOT NULL DEFAULT 0,
	enabled      INTEGER NOT NULL DEFAULT 1,
	admin_notes  TEXT NOT NULL DEFAULT '',
	cid          TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS codes_by_public_enabled
	ON codes (public, enabled, created_at DESC);`

const codeCols = `id, label, mode, target, ec, public, enabled, admin_notes, ` +
	`cid, created_at, updated_at`

// openDB opens the SQLite database, applies pragmas, runs schema, and
// performs idempotent column-add migrations. See the self-migrating-sqlite
// skill — every step is safe to run on every startup.
func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if _, err := db.Exec(store.SessionSchema); err != nil {
		return nil, err
	}
	for _, c := range []struct{ col, decl string }{
		{"label", "TEXT NOT NULL DEFAULT ''"},
		{"mode", "TEXT NOT NULL DEFAULT 'direct'"},
		{"target", "TEXT NOT NULL DEFAULT ''"},
		{"ec", "TEXT NOT NULL DEFAULT 'M'"},
		{"public", "INTEGER NOT NULL DEFAULT 0"},
		{"enabled", "INTEGER NOT NULL DEFAULT 1"},
		{"admin_notes", "TEXT NOT NULL DEFAULT ''"},
		{"cid", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(db, "codes", c.col, c.decl); err != nil {
			return nil, err
		}
	}
	if err := backfillCIDs(db); err != nil {
		return nil, err
	}
	return db, nil
}

func ensureColumn(db *sql.DB, table, column, decl string) error {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + decl)
	return err
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// codeCID hashes the public-facing configuration of a Code. The id, admin
// notes, and timestamps are deliberately excluded so admin-only edits and
// updated_at churn do not bump the CID and invalidate ETags.
func codeCID(c *Code) string {
	return cid.OfValue(map[string]any{
		"label":   c.Label,
		"mode":    string(c.Mode),
		"target":  c.Target,
		"ec":      strings.ToUpper(c.EC),
		"public":  c.Public,
		"enabled": c.Enabled,
	})
}

// publicView is a copy of the code with admin-only fields cleared, safe to
// return from the public read API.
func publicView(c *Code) *Code {
	out := *c
	out.AdminNotes = ""
	return &out
}

// publicList is publicView applied across a slice.
func publicList(cs []Code) []Code {
	out := make([]Code, len(cs))
	for i := range cs {
		out[i] = cs[i]
		out[i].AdminNotes = ""
	}
	return out
}

// scanner accepts both *sql.Row and *sql.Rows.
type scanner interface{ Scan(...any) error }

func scanCode(row scanner) (*Code, error) {
	var c Code
	var public, enabled int
	if err := row.Scan(&c.ID, &c.Label, &c.Mode, &c.Target, &c.EC,
		&public, &enabled, &c.AdminNotes, &c.CID,
		&c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Public = public != 0
	c.Enabled = enabled != 0
	return &c, nil
}

func normalize(c *Code) {
	c.Target = strings.TrimSpace(c.Target)
	c.Label = strings.TrimSpace(c.Label)
	c.AdminNotes = strings.TrimSpace(c.AdminNotes)
	if c.Mode == "" {
		c.Mode = ModeDirect
	}
	c.EC = strings.ToUpper(strings.TrimSpace(c.EC))
	if c.EC == "" {
		c.EC = "M"
	}
}

// insertCode creates a code, assigning a fresh id, CID, and timestamps. The
// Mode and EC fields must already be validated by the caller.
func insertCode(db *sql.DB, c *Code) error {
	if c.ID == "" {
		c.ID = store.ShortID()
	}
	normalize(c)
	c.CreatedAt = nowRFC3339()
	c.UpdatedAt = c.CreatedAt
	c.CID = codeCID(c)
	_, err := db.Exec(
		`INSERT INTO codes (`+codeCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Label, string(c.Mode), c.Target, c.EC,
		boolToInt(c.Public), boolToInt(c.Enabled), c.AdminNotes,
		c.CID, c.CreatedAt, c.UpdatedAt)
	return err
}

// updateCode replaces a code in place, recomputing CID and updated_at. The id
// and created_at are not read from c.
func updateCode(db *sql.DB, id string, c *Code) (bool, error) {
	normalize(c)
	c.UpdatedAt = nowRFC3339()
	c.CID = codeCID(c)
	res, err := db.Exec(
		`UPDATE codes SET label = ?, mode = ?, target = ?, ec = ?,
			public = ?, enabled = ?, admin_notes = ?, cid = ?, updated_at = ?
			WHERE id = ?`,
		c.Label, string(c.Mode), c.Target, c.EC,
		boolToInt(c.Public), boolToInt(c.Enabled), c.AdminNotes,
		c.CID, c.UpdatedAt, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	c.ID = id
	return true, nil
}

// getCode returns a code by id, or (nil, nil) when absent.
func getCode(db *sql.DB, id string) (*Code, error) {
	c, err := scanCode(db.QueryRow(
		`SELECT `+codeCols+` FROM codes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// listCodes returns every code, newest first.
func listCodes(db *sql.DB) ([]Code, error) {
	rows, err := db.Query(
		`SELECT ` + codeCols + ` FROM codes ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Code{}
	for rows.Next() {
		c, err := scanCode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// listPublicCodes returns codes that are both public AND enabled — the
// records the JSON read API and public QR endpoints expose.
func listPublicCodes(db *sql.DB) ([]Code, error) {
	rows, err := db.Query(
		`SELECT ` + codeCols + ` FROM codes WHERE public = 1 AND enabled = 1 ` +
			`ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Code{}
	for rows.Next() {
		c, err := scanCode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func deleteCode(db *sql.DB, id string) (bool, error) {
	res, err := db.Exec(`DELETE FROM codes WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// backfillCIDs computes the CID for any code that lacks one — a one-time
// migration for pre-CID databases.
func backfillCIDs(db *sql.DB) error {
	rows, err := db.Query(`SELECT ` + codeCols + ` FROM codes WHERE cid = ''`)
	if err != nil {
		return err
	}
	var cs []Code
	for rows.Next() {
		c, err := scanCode(rows)
		if err != nil {
			rows.Close()
			return err
		}
		cs = append(cs, *c)
	}
	rows.Close()
	for i := range cs {
		if _, err := db.Exec(`UPDATE codes SET cid = ? WHERE id = ?`,
			codeCID(&cs[i]), cs[i].ID); err != nil {
			return err
		}
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
