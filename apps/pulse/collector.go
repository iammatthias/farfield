package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iammatthias/farfield/lib/store"
)

// startCollector runs the traffic roll-up loop: every interval it sweeps the
// sibling app databases for new lib/pulse request rows and folds them into
// pulse's daily aggregates. Each pass runs once immediately on start.
func startCollector(db *sql.DB, dbPath string, interval time.Duration) {
	go func() {
		for {
			collectAll(db, dbPath)
			time.Sleep(interval)
		}
	}()
}

// collectAll discovers sibling app databases the farfield way: every
// *.sqlite beside pulse's own database is an app named by its filename stem
// (the same convention the backup app's snapshot targets use). Pulse's own
// database is skipped; so is any database without a `requests` table —
// that app simply has not adopted the lib/pulse middleware yet.
func collectAll(db *sql.DB, dbPath string) {
	own, err := filepath.Abs(dbPath)
	if err != nil {
		own = filepath.Clean(dbPath)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "*.sqlite"))
	sort.Strings(matches)
	for _, p := range matches {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = filepath.Clean(p)
		}
		if abs == own {
			continue // pulse's own database
		}
		app := strings.TrimSuffix(filepath.Base(p), ".sqlite")
		if err := collectApp(db, app, p); err != nil {
			slog.Warn("collector: app sweep failed", "app", app, "err", err)
		}
	}
	pruneVKeys(db)
}

// hitKey addresses one hits_daily counter cell.
type hitKey struct{ day, path, method, bucket string }

// refKey addresses one referrers_daily counter cell.
type refKey struct{ day, host string }

// pathKey is the granularity at which uniques are exact (matches vkeys_seen).
type pathKey struct{ day, path string }

// collectApp reads the app's requests rows past the stored cursor (the
// source is opened read-only), aggregates them in memory, and applies the
// roll-up to pulse's database in one transaction: counter upserts into
// hits_daily/referrers_daily, exact-unique accounting through vkeys_seen,
// and the cursor advanced to the highest id read. Because cursor and
// counters commit atomically, a crash either replays nothing or everything —
// never half a batch — and re-running over the same rows cannot
// double-count.
func collectApp(db *sql.DB, app, path string) error {
	src, err := sql.Open("sqlite",
		"file:"+path+"?mode=ro&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return err
	}
	defer src.Close()

	var n int
	if err := src.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'requests'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil // app has not adopted lib/pulse yet — skip silently
	}

	var cursor int64
	err = db.QueryRow(`SELECT last_event_id FROM collector_cursor
		WHERE app = ?`, app).Scan(&cursor)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	rows, err := src.Query(`SELECT id, ts, path, method, status, vkey, ref_host
		FROM requests WHERE id > ? ORDER BY id`, cursor)
	if err != nil {
		return err
	}
	defer rows.Close()

	hits := make(map[hitKey]int)
	refs := make(map[refKey]int)
	// vkeys[pathKey][vkey] remembers which hits_daily cell first saw the
	// vkey in this batch — a confirmed-new visitor increments that cell.
	vkeys := make(map[pathKey]map[string]hitKey)
	maxID := cursor
	for rows.Next() {
		var id int64
		var status int
		var ts, reqPath, method, vkey, refHost string
		if err := rows.Scan(&id, &ts, &reqPath, &method, &status, &vkey, &refHost); err != nil {
			return err
		}
		maxID = id
		day := tsDay(ts)
		hk := hitKey{day, reqPath, method, statusBucket(status)}
		hits[hk]++
		if vkey != "" {
			pk := pathKey{day, reqPath}
			if vkeys[pk] == nil {
				vkeys[pk] = make(map[string]hitKey)
			}
			if _, dup := vkeys[pk][vkey]; !dup {
				vkeys[pk][vkey] = hk
			}
		}
		if refHost != "" {
			refs[refKey{day, refHost}]++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if maxID == cursor {
		return nil // nothing new
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Exact uniques across batches: INSERT OR IGNORE into vkeys_seen; only a
	// first-ever insertion (rows affected = 1) counts the visitor, so a vkey
	// already recorded by an earlier collector run adds nothing.
	uniques := make(map[hitKey]int)
	for pk, perVkey := range vkeys {
		for vkey, hk := range perVkey {
			res, err := tx.Exec(`INSERT OR IGNORE INTO vkeys_seen
				(day, app, path, vkey) VALUES (?, ?, ?, ?)`,
				pk.day, app, pk.path, vkey)
			if err != nil {
				return err
			}
			if changed, err := res.RowsAffected(); err != nil {
				return err
			} else if changed == 1 {
				uniques[hk]++
			}
		}
	}
	for hk, count := range hits {
		if _, err := tx.Exec(`INSERT INTO hits_daily
			(day, app, path, method, status_bucket, hits, uniques)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(day, app, path, method, status_bucket) DO UPDATE SET
			hits = hits + excluded.hits, uniques = uniques + excluded.uniques`,
			hk.day, app, hk.path, hk.method, hk.bucket, count, uniques[hk]); err != nil {
			return err
		}
	}
	for rk, count := range refs {
		if _, err := tx.Exec(`INSERT INTO referrers_daily
			(day, app, referrer_host, hits) VALUES (?, ?, ?, ?)
			ON CONFLICT(day, app, referrer_host) DO UPDATE SET
			hits = hits + excluded.hits`,
			rk.day, app, rk.host, count); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO collector_cursor
		(app, last_event_id, last_run) VALUES (?, ?, ?)
		ON CONFLICT(app) DO UPDATE SET
		last_event_id = excluded.last_event_id, last_run = excluded.last_run`,
		app, maxID, store.NowRFC3339()); err != nil {
		return err
	}
	return tx.Commit()
}

// pruneVKeys drops vkeys_seen rows older than yesterday (UTC). lib/pulse
// vkeys rotate at UTC midnight, so only today's and yesterday's rows can
// still receive events — anything older is dead weight.
func pruneVKeys(db *sql.DB) {
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	if _, err := db.Exec(`DELETE FROM vkeys_seen WHERE day < ?`, yesterday); err != nil {
		slog.Warn("collector: vkeys_seen prune failed", "err", err)
	}
}

// tsDay extracts the UTC day from an RFC 3339 timestamp; lib/pulse always
// writes UTC, so the date prefix is the day.
func tsDay(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// statusBucket maps a status code to its class: "2xx", "3xx", … Transport
// failures and garbage land in "other".
func statusBucket(status int) string {
	if status < 100 || status > 599 {
		return "other"
	}
	return fmt.Sprintf("%dxx", status/100)
}
