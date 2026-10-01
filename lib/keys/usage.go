package keys

// Usage rollups: how often each key is presented, where, and whether it was
// accepted. Check runs on every authenticated request across the fleet
// against one shared SQLite file, so — like last_used_at — usage must never
// cost a write per request. Each process counts in memory and flushes its
// rollups in one transaction per flushInterval (and on Close): a handful of
// writes a minute fleet-wide, whatever the traffic.
//
// What is kept is deliberately thin: the key's id, the app, a UTC day, the
// method and route, the outcome, a count and the last time. Never a token, a
// hash of an unknown token, an IP, a query string or a body. A token that
// names no key is not recorded at all.

import (
	"log/slog"
	"strings"
	"time"
)

const usageSchema = `
CREATE TABLE IF NOT EXISTS key_usage (
	key_id  TEXT NOT NULL,
	app     TEXT NOT NULL,
	day     TEXT NOT NULL,
	method  TEXT NOT NULL,
	path    TEXT NOT NULL,
	outcome TEXT NOT NULL,
	count   INTEGER NOT NULL,
	last_at TEXT NOT NULL,
	PRIMARY KEY (key_id, day, app, method, path, outcome)
);
CREATE INDEX IF NOT EXISTS key_usage_day ON key_usage (day);`

// Outcomes of presenting a token that names a key. Anything but OutcomeOK is
// a refusal — and a signal: something out there still holds a dead key.
const (
	OutcomeOK       = "ok"
	OutcomeRevoked  = "revoked"
	OutcomeExpired  = "expired"
	OutcomeWrongApp = "wrong-app"
)

// flushInterval is how often a process writes its pending rollups. A var so
// tests can stretch it; usageRetention bounds how long rollups are kept.
var (
	flushInterval  = time.Minute
	usageRetention = 90 * 24 * time.Hour
)

const (
	// maxPending caps the distinct rollups one process holds between
	// flushes; past it, new routes collapse into otherPath so a caller
	// cycling paths cannot grow the map without bound.
	maxPending = 2000
	otherPath  = "(other)"
	maxPathLen = 120
	dayLayout  = "2006-01-02"
)

type usageKey struct {
	keyID, app, day, method, path, outcome string
}

type usageAgg struct {
	count  int64
	lastAt time.Time
}

// record counts one presentation in memory. No database work happens here.
func (s *Store) record(keyID, app, method, path, outcome string, now time.Time) {
	k := usageKey{
		keyID:   keyID,
		app:     app,
		day:     now.UTC().Format(dayLayout),
		method:  cleanMethod(method),
		path:    cleanPath(path),
		outcome: outcome,
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	a, ok := s.usage[k]
	if !ok && len(s.usage) >= maxPending {
		k.path = otherPath
		a, ok = s.usage[k]
	}
	if !ok {
		a = &usageAgg{}
		s.usage[k] = a
	}
	a.count++
	if now.After(a.lastAt) {
		a.lastAt = now
	}
}

// cleanPath keeps only the route path: no query, bounded length.
func cleanPath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if len(p) > maxPathLen {
		p = strings.ToValidUTF8(p[:maxPathLen], "") + "…"
	}
	return p
}

func cleanMethod(m string) string {
	m = strings.ToUpper(m)
	if len(m) > 10 {
		m = m[:10]
	}
	return m
}

func (s *Store) flushLoop() {
	defer close(s.flushed)
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if err := s.Flush(); err != nil {
				slog.Warn("keys: usage flush failed", "err", err)
			}
		}
	}
}

// Flush writes the pending rollups in one transaction, merging them into the
// stored counts, and prunes rollups past usageRetention. Nothing pending
// means no write at all. On failure the batch goes back into memory for the
// next attempt. Rollups for a key deleted meanwhile are dropped.
func (s *Store) Flush() error {
	s.usageMu.Lock()
	batch := s.usage
	s.usage = make(map[usageKey]*usageAgg, len(batch))
	s.usageMu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	if err := s.writeUsage(batch); err != nil {
		s.requeue(batch)
		return err
	}
	return nil
}

func (s *Store) writeUsage(batch map[usageKey]*usageAgg) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The trailing WHERE also settles SQLite's INSERT … SELECT … ON CONFLICT
	// parsing ambiguity.
	stmt, err := tx.Prepare(`INSERT INTO key_usage
		(key_id, app, day, method, path, outcome, count, last_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM api_keys WHERE id = ?)
		ON CONFLICT (key_id, day, app, method, path, outcome) DO UPDATE SET
			count   = count + excluded.count,
			last_at = max(last_at, excluded.last_at)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, a := range batch {
		if _, err := stmt.Exec(k.keyID, k.app, k.day, k.method, k.path, k.outcome,
			a.count, a.lastAt.UTC().Format(time.RFC3339), k.keyID); err != nil {
			return err
		}
	}
	cutoff := time.Now().UTC().Add(-usageRetention).Format(dayLayout)
	if _, err := tx.Exec(`DELETE FROM key_usage WHERE day < ?`, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

// requeue merges a batch that failed to write back into the pending map.
func (s *Store) requeue(batch map[usageKey]*usageAgg) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	for k, a := range batch {
		if cur, ok := s.usage[k]; ok {
			cur.count += a.count
			if a.lastAt.After(cur.lastAt) {
				cur.lastAt = a.lastAt
			}
			continue
		}
		if len(s.usage) >= maxPending {
			continue // over the cap even after collapsing; drop rather than grow
		}
		s.usage[k] = a
	}
}

// AppUsage is one app's totals for a key.
type AppUsage struct {
	App     string
	OK      int64
	Refused int64
	LastAt  string
}

// DayUsage is one UTC day's counts for a key.
type DayUsage struct {
	Day     string
	OK      int64
	Refused int64
}

// PathUsage is one route a key was presented on, summed across days.
type PathUsage struct {
	App     string
	Method  string
	Path    string
	Outcome string
	Count   int64
	LastAt  string
}

// Usage is a key's flushed rollups, for its page. ByApp, Recent and the
// totals cover everything retained (usageRetention); Days covers the last
// len(Days) days, oldest first, with zero days filled in.
type Usage struct {
	OK      int64
	Refused int64
	ByApp   []AppUsage
	Days    []DayUsage
	Recent  []PathUsage
}

// Usage reads a key's rollups: the last days days, and up to recent routes,
// most recent first. Only flushed rollups are visible — every process holds
// up to flushInterval of counts in memory.
func (s *Store) Usage(id string, days, recent int) (*Usage, error) {
	u := &Usage{}

	rows, err := s.db.Query(`SELECT app,
		SUM(CASE WHEN outcome = 'ok' THEN count ELSE 0 END),
		SUM(CASE WHEN outcome = 'ok' THEN 0 ELSE count END),
		MAX(last_at)
		FROM key_usage WHERE key_id = ? GROUP BY app ORDER BY MAX(last_at) DESC`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a AppUsage
		if err := rows.Scan(&a.App, &a.OK, &a.Refused, &a.LastAt); err != nil {
			rows.Close()
			return nil, err
		}
		u.OK += a.OK
		u.Refused += a.Refused
		u.ByApp = append(u.ByApp, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	today := time.Now().UTC()
	first := today.AddDate(0, 0, -(days - 1)).Format(dayLayout)
	byDay := make(map[string]DayUsage, days)
	rows, err = s.db.Query(`SELECT day,
		SUM(CASE WHEN outcome = 'ok' THEN count ELSE 0 END),
		SUM(CASE WHEN outcome = 'ok' THEN 0 ELSE count END)
		FROM key_usage WHERE key_id = ? AND day >= ? GROUP BY day`, id, first)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d DayUsage
		if err := rows.Scan(&d.Day, &d.OK, &d.Refused); err != nil {
			rows.Close()
			return nil, err
		}
		byDay[d.Day] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := days - 1; i >= 0; i-- {
		day := today.AddDate(0, 0, -i).Format(dayLayout)
		d := byDay[day]
		d.Day = day
		u.Days = append(u.Days, d)
	}

	rows, err = s.db.Query(`SELECT app, method, path, outcome, SUM(count), MAX(last_at)
		FROM key_usage WHERE key_id = ?
		GROUP BY app, method, path, outcome
		ORDER BY MAX(last_at) DESC LIMIT ?`, id, recent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p PathUsage
		if err := rows.Scan(&p.App, &p.Method, &p.Path, &p.Outcome, &p.Count, &p.LastAt); err != nil {
			return nil, err
		}
		u.Recent = append(u.Recent, p)
	}
	return u, rows.Err()
}
