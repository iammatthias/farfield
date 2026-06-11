package main

import "database/sql"

// Traffic page queries — all served from the daily aggregate tables the
// collector maintains, never from raw request rows.
//
// A note on uniques: hits_daily.uniques is exact per (day, app, path) —
// see vkeys_seen in db.go. Day-level "uniques" figures are sums of those
// per-path counts, so a visitor who hit three paths counts three times.
// The label in the UI says "path-uniques" for that reason.

// DayCount is one (day, n) pair for the charts.
type DayCount struct {
	Day string `json:"day"`
	N   int    `json:"n"`
}

// appFilter builds the optional app predicate. The app value is bound as a
// parameter, never interpolated.
func appFilter(app string) (clause string, args []any) {
	if app == "" {
		return "", nil
	}
	return " AND app = ?", []any{app}
}

func queryDayCounts(db *sql.DB, query string, args ...any) ([]DayCount, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayCount
	for rows.Next() {
		var d DayCount
		if err := rows.Scan(&d.Day, &d.N); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// hitsPerDay returns total hits per day in [from, to], optionally for one app.
func hitsPerDay(db *sql.DB, app, from, to string) ([]DayCount, error) {
	clause, extra := appFilter(app)
	return queryDayCounts(db, `SELECT day, SUM(hits) FROM hits_daily
		WHERE day >= ? AND day <= ?`+clause+` GROUP BY day ORDER BY day`,
		append([]any{from, to}, extra...)...)
}

// uniquesPerDay returns summed per-path uniques per day (see the package
// note above on what that means).
func uniquesPerDay(db *sql.DB, app, from, to string) ([]DayCount, error) {
	clause, extra := appFilter(app)
	return queryDayCounts(db, `SELECT day, SUM(uniques) FROM hits_daily
		WHERE day >= ? AND day <= ?`+clause+` GROUP BY day ORDER BY day`,
		append([]any{from, to}, extra...)...)
}

// PathStat is one row of the top-paths table.
type PathStat struct {
	App     string `json:"app"`
	Path    string `json:"path"`
	Hits    int    `json:"hits"`
	Uniques int    `json:"uniques"`
}

func topPaths(db *sql.DB, app, from, to string, n int) ([]PathStat, error) {
	clause, extra := appFilter(app)
	args := append([]any{from, to}, extra...)
	rows, err := db.Query(`SELECT app, path, SUM(hits), SUM(uniques)
		FROM hits_daily WHERE day >= ? AND day <= ?`+clause+`
		GROUP BY app, path ORDER BY SUM(hits) DESC LIMIT ?`,
		append(args, n)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PathStat
	for rows.Next() {
		var p PathStat
		if err := rows.Scan(&p.App, &p.Path, &p.Hits, &p.Uniques); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BucketStat is one slice of the status-class mix.
type BucketStat struct {
	Bucket string `json:"bucket"`
	Hits   int    `json:"hits"`
}

func statusMix(db *sql.DB, app, from, to string) ([]BucketStat, error) {
	clause, extra := appFilter(app)
	rows, err := db.Query(`SELECT status_bucket, SUM(hits) FROM hits_daily
		WHERE day >= ? AND day <= ?`+clause+`
		GROUP BY status_bucket ORDER BY status_bucket`,
		append([]any{from, to}, extra...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BucketStat
	for rows.Next() {
		var b BucketStat
		if err := rows.Scan(&b.Bucket, &b.Hits); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RefStat is one row of the top-referrers table.
type RefStat struct {
	Host string `json:"host"`
	Hits int    `json:"hits"`
}

func topReferrers(db *sql.DB, app, from, to string, n int) ([]RefStat, error) {
	clause, extra := appFilter(app)
	args := append([]any{from, to}, extra...)
	rows, err := db.Query(`SELECT referrer_host, SUM(hits) FROM referrers_daily
		WHERE day >= ? AND day <= ?`+clause+`
		GROUP BY referrer_host ORDER BY SUM(hits) DESC LIMIT ?`,
		append(args, n)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefStat
	for rows.Next() {
		var rs RefStat
		if err := rows.Scan(&rs.Host, &rs.Hits); err != nil {
			return nil, err
		}
		out = append(out, rs)
	}
	return out, rows.Err()
}

// trafficApps lists the apps that have any aggregated traffic — the filter
// bar's options.
func trafficApps(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT app FROM hits_daily ORDER BY app`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
