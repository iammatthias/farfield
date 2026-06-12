package main

import (
	"database/sql"
	"errors"
	"time"

	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// schema is pulse's current database shape, applied on every open.
//
//   - targets/checks/incidents back the uptime checker. checks is
//     append-only; the (target_id, id DESC) index serves "latest check" and
//     sparkline reads, the (target_id, ts DESC) index serves the uptime
//     windows.
//   - hits_daily/referrers_daily/collector_cursor back the traffic
//     collector. vkeys_seen is the auxiliary exact-uniques table: counters
//     alone cannot keep distinct-vkey-per-(day,app,path) exact across
//     collector runs, so each (day,app,path,vkey) is INSERT OR IGNOREd and
//     only first insertions increment a uniques counter. Rows older than
//     yesterday are pruned — vkeys rotate daily, so only today and yesterday
//     can still receive events.
const schema = `
CREATE TABLE IF NOT EXISTS targets (
	id              INTEGER PRIMARY KEY,
	name            TEXT    NOT NULL,
	url             TEXT    NOT NULL,
	method          TEXT    NOT NULL DEFAULT 'GET',
	expected_status INTEGER NOT NULL DEFAULT 200,
	interval_s      INTEGER NOT NULL DEFAULT 60,
	enabled         INTEGER NOT NULL DEFAULT 1,
	created_at      TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS checks (
	id          INTEGER PRIMARY KEY,
	target_id   INTEGER NOT NULL,
	ts          TEXT    NOT NULL,
	status_code INTEGER NOT NULL DEFAULT 0,
	latency_ms  INTEGER NOT NULL DEFAULT 0,
	ok          INTEGER NOT NULL,
	err         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS checks_target_ts ON checks(target_id, ts DESC);
CREATE INDEX IF NOT EXISTS checks_target_id ON checks(target_id, id DESC);

CREATE TABLE IF NOT EXISTS incidents (
	id        INTEGER PRIMARY KEY,
	target_id INTEGER NOT NULL,
	opened_at TEXT    NOT NULL,
	closed_at TEXT    NOT NULL DEFAULT '',
	last_err  TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS incidents_target_closed ON incidents(target_id, closed_at);

CREATE TABLE IF NOT EXISTS hits_daily (
	day           TEXT    NOT NULL,
	app           TEXT    NOT NULL,
	path          TEXT    NOT NULL,
	method        TEXT    NOT NULL,
	status_bucket TEXT    NOT NULL,
	hits          INTEGER NOT NULL DEFAULT 0,
	uniques       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, app, path, method, status_bucket)
);

CREATE TABLE IF NOT EXISTS referrers_daily (
	day           TEXT    NOT NULL,
	app           TEXT    NOT NULL,
	referrer_host TEXT    NOT NULL,
	hits          INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, app, referrer_host)
);

CREATE TABLE IF NOT EXISTS collector_cursor (
	app           TEXT PRIMARY KEY,
	last_event_id INTEGER NOT NULL DEFAULT 0,
	last_run      TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS vkeys_seen (
	day  TEXT NOT NULL,
	app  TEXT NOT NULL,
	path TEXT NOT NULL,
	vkey TEXT NOT NULL,
	PRIMARY KEY (day, app, path, vkey)
);`

// openDB opens (or creates) the pulse database and brings its schema
// current. Every step is idempotent, so this runs on every startup — see the
// self-migrating-sqlite discipline. Future column additions go through
// store.EnsureColumn / store.RenameColumn here, in rename → add → backfill
// order.
func openDB(path string) (*sql.DB, error) {
	db, err := store.OpenDB(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(store.SessionSchema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ── targets ────────────────────────────────────────────────────────────────

// Target is one monitored HTTP endpoint.
type Target struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	Method         string `json:"method"`
	ExpectedStatus int    `json:"expectedStatus"`
	IntervalS      int    `json:"intervalS"`
	Enabled        bool   `json:"enabled"`
	CreatedAt      string `json:"createdAt"`
}

func scanTargets(rows *sql.Rows) ([]Target, error) {
	defer rows.Close()
	var out []Target
	for rows.Next() {
		var t Target
		var enabled int
		if err := rows.Scan(&t.ID, &t.Name, &t.URL, &t.Method,
			&t.ExpectedStatus, &t.IntervalS, &enabled, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Enabled = enabled != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

const targetCols = `id, name, url, method, expected_status, interval_s, enabled, created_at`

func listTargets(db *sql.DB) ([]Target, error) {
	rows, err := db.Query(`SELECT ` + targetCols + ` FROM targets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return scanTargets(rows)
}

func listEnabledTargets(db *sql.DB) ([]Target, error) {
	rows, err := db.Query(`SELECT ` + targetCols + ` FROM targets WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	return scanTargets(rows)
}

func getTarget(db *sql.DB, id int64) (*Target, error) {
	var t Target
	var enabled int
	err := db.QueryRow(`SELECT `+targetCols+` FROM targets WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.URL, &t.Method, &t.ExpectedStatus,
			&t.IntervalS, &enabled, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Enabled = enabled != 0
	return &t, nil
}

func insertTarget(db *sql.DB, t *Target) error {
	t.CreatedAt = store.NowRFC3339()
	res, err := db.Exec(`INSERT INTO targets
		(name, url, method, expected_status, interval_s, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.URL, t.Method, t.ExpectedStatus, t.IntervalS,
		boolInt(t.Enabled), t.CreatedAt)
	if err != nil {
		return err
	}
	t.ID, err = res.LastInsertId()
	return err
}

func updateTarget(db *sql.DB, t *Target) (bool, error) {
	res, err := db.Exec(`UPDATE targets SET name = ?, url = ?, method = ?,
		expected_status = ?, interval_s = ?, enabled = ? WHERE id = ?`,
		t.Name, t.URL, t.Method, t.ExpectedStatus, t.IntervalS,
		boolInt(t.Enabled), t.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// deleteTarget removes a target and its check/incident history.
func deleteTarget(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM checks WHERE target_id = ?`,
		`DELETE FROM incidents WHERE target_id = ?`,
		`DELETE FROM targets WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func countTargets(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM targets`).Scan(&n)
	return n, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── checks & incidents ─────────────────────────────────────────────────────

// checkResult is the outcome of one probe of a target.
type checkResult struct {
	StatusCode int
	LatencyMS  int64
	OK         bool
	Err        string
}

// recordCheck appends a checks row and applies the incident state machine in
// one transaction. Every probe outcome is recorded honestly — the checks
// table and the uptime percentages always reflect the raw results — but the
// open transition is debounced: an incident opens only once failThreshold
// CONSECUTIVE checks (the one being recorded included) have failed. With the
// default threshold of 2, a single flaked probe still shows as a failed
// check, yet never becomes an incident; production sees regular one-probe
// "context deadline exceeded" blips on the Cloudflare-tunnel hairpin path
// that are not real outages.
//
//	fail, incident open   : refresh its last_err
//	fail, no incident     : open one iff the newest failThreshold checks
//	                        (this one included) all failed
//	ok,   incident open   : close it — recovery is never debounced
//	ok,   no incident     : nothing
//
// With failThreshold = 1 this is the original behavior: a first-ever failing
// check opens an incident immediately. The consecutive-fail window is read
// back from the checks table itself (one LIMIT-K read on the
// (target_id, id DESC) index, and only on failed probes that have no open
// incident), so the debounce state survives restarts instead of living in
// checker memory.
func recordCheck(db *sql.DB, targetID int64, res checkResult, failThreshold int) error {
	if failThreshold < 1 {
		failThreshold = 1
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := store.NowRFC3339()
	if _, err := tx.Exec(`INSERT INTO checks
		(target_id, ts, status_code, latency_ms, ok, err)
		VALUES (?, ?, ?, ?, ?, ?)`,
		targetID, now, res.StatusCode, res.LatencyMS, boolInt(res.OK), res.Err); err != nil {
		return err
	}

	if res.OK {
		// First good check closes any open incident.
		if _, err := tx.Exec(`UPDATE incidents SET closed_at = ?
			WHERE target_id = ? AND closed_at = ''`,
			now, targetID); err != nil {
			return err
		}
		return tx.Commit()
	}

	// Failed check: keep an already-open incident's last_err current…
	upd, err := tx.Exec(`UPDATE incidents SET last_err = ?
		WHERE target_id = ? AND closed_at = ''`,
		res.Err, targetID)
	if err != nil {
		return err
	}
	if n, err := upd.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return tx.Commit()
	}

	// …or open one once the newest failThreshold checks all failed. fails
	// can only equal failThreshold when at least that many checks exist.
	var fails int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM (
			SELECT ok FROM checks WHERE target_id = ?
			ORDER BY id DESC LIMIT ?) WHERE ok = 0`,
		targetID, failThreshold).Scan(&fails); err != nil {
		return err
	}
	if fails >= failThreshold {
		if _, err := tx.Exec(`INSERT INTO incidents
			(target_id, opened_at, last_err) VALUES (?, ?, ?)`,
			targetID, now, res.Err); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// latestCheck returns the most recent check for a target, or nil when the
// target has never been probed.
type Check struct {
	TS         string `json:"ts"`
	StatusCode int    `json:"statusCode"`
	LatencyMS  int64  `json:"latencyMs"`
	OK         bool   `json:"ok"`
	Err        string `json:"err,omitempty"`
}

func latestCheck(db *sql.DB, targetID int64) (*Check, error) {
	var c Check
	var ok int
	err := db.QueryRow(`SELECT ts, status_code, latency_ms, ok, err
		FROM checks WHERE target_id = ? ORDER BY id DESC LIMIT 1`, targetID).
		Scan(&c.TS, &c.StatusCode, &c.LatencyMS, &ok, &c.Err)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.OK = ok != 0
	return &c, nil
}

// recentChecks returns the last n checks for a target, oldest first — the
// sparkline's input.
func recentChecks(db *sql.DB, targetID int64, n int) ([]Check, error) {
	rows, err := db.Query(`SELECT ts, status_code, latency_ms, ok, err
		FROM checks WHERE target_id = ? ORDER BY id DESC LIMIT ?`, targetID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		var c Check
		var ok int
		if err := rows.Scan(&c.TS, &c.StatusCode, &c.LatencyMS, &ok, &c.Err); err != nil {
			return nil, err
		}
		c.OK = ok != 0
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// uptime returns the ok percentage over the window ending now, computed in
// SQL from the checks table. ok=false means no checks fell in the window.
func uptime(db *sql.DB, targetID int64, window time.Duration) (pct float64, ok bool, err error) {
	since := time.Now().UTC().Add(-window).Format(time.RFC3339)
	var total, up int
	err = db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(ok), 0) FROM checks
		WHERE target_id = ? AND ts >= ?`, targetID, since).Scan(&total, &up)
	if err != nil || total == 0 {
		return 0, false, err
	}
	return 100 * float64(up) / float64(total), true, nil
}

// Incident is one outage span; ClosedAt is "" while it is still open.
type Incident struct {
	ID         int64  `json:"id"`
	TargetID   int64  `json:"targetId"`
	TargetName string `json:"targetName,omitempty"`
	OpenedAt   string `json:"openedAt"`
	ClosedAt   string `json:"closedAt"`
	LastErr    string `json:"lastErr"`
}

// openIncident returns the target's currently open incident, or nil.
func openIncident(db *sql.DB, targetID int64) (*Incident, error) {
	var inc Incident
	err := db.QueryRow(`SELECT id, target_id, opened_at, closed_at, last_err
		FROM incidents WHERE target_id = ? AND closed_at = ''
		ORDER BY id DESC LIMIT 1`, targetID).
		Scan(&inc.ID, &inc.TargetID, &inc.OpenedAt, &inc.ClosedAt, &inc.LastErr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inc, nil
}

// recentIncidents returns the latest incidents across all targets, newest
// first, with target names joined in.
func recentIncidents(db *sql.DB, n int) ([]Incident, error) {
	rows, err := db.Query(`SELECT i.id, i.target_id,
		COALESCE(t.name, '#' || i.target_id), i.opened_at, i.closed_at, i.last_err
		FROM incidents i LEFT JOIN targets t ON t.id = i.target_id
		ORDER BY i.id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		var inc Incident
		if err := rows.Scan(&inc.ID, &inc.TargetID, &inc.TargetName,
			&inc.OpenedAt, &inc.ClosedAt, &inc.LastErr); err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}
