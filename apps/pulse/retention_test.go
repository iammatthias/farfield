package main

import (
	"testing"
	"time"
)

// TestPruneTrafficKeepsEverythingInsideTheWindow is the safety half of the
// retention change: these tables had no bound at all, so switching one on is
// the moment to prove it removes only what it promises. A row inside the
// window must survive; the console's longest view is 30 days and the window
// is 180, so anything a human can see is far from the edge.
func TestPruneTrafficKeepsEverythingInsideTheWindow(t *testing.T) {
	db := newTestDB(t, t.TempDir(), "pulse.sqlite")

	day := func(d int) string {
		return time.Now().UTC().AddDate(0, 0, d).Format("2006-01-02")
	}
	rows := map[string]bool{ // day -> should survive
		day(0):    true,  // today
		day(-30):  true,  // edge of the console's longest window
		day(-179): true,  // just inside retention
		day(-181): false, // just outside
		day(-400): false, // long past
	}
	for d := range rows {
		if _, err := db.Exec(`INSERT INTO hits_daily (day, app, path, method, status_bucket, hits, uniques)
			VALUES (?, 'content', '/', 'GET', '2xx', 1, 1)`, d); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO referrers_daily (day, app, referrer_host, hits)
			VALUES (?, 'content', 'example.com', 1)`, d); err != nil {
			t.Fatal(err)
		}
	}

	pruneTraffic(db)

	for d, shouldSurvive := range rows {
		for _, table := range []string{"hits_daily", "referrers_daily"} {
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE day = ?`, d).Scan(&n); err != nil {
				t.Fatal(err)
			}
			switch {
			case shouldSurvive && n == 0:
				t.Errorf("%s: day %s was deleted but is inside the %v window", table, d, trafficRetention)
			case !shouldSurvive && n != 0:
				t.Errorf("%s: day %s survived but is outside the window", table, d)
			}
		}
	}
}

// TestPruneTrafficIsANoopOnCurrentData — nothing in production is older than
// the window, so enabling this must delete zero rows there. Modelled here with
// a table holding only recent days.
func TestPruneTrafficIsANoopOnRecentData(t *testing.T) {
	db := newTestDB(t, t.TempDir(), "pulse.sqlite")
	for i := 0; i < 70; i++ { // ten weeks, the real span when this was written
		d := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		if _, err := db.Exec(`INSERT INTO hits_daily (day, app, path, method, status_bucket, hits, uniques)
			VALUES (?, 'content', '/', 'GET', '2xx', 1, 1)`, d); err != nil {
			t.Fatal(err)
		}
	}
	var before, after int
	db.QueryRow(`SELECT COUNT(*) FROM hits_daily`).Scan(&before)
	pruneTraffic(db)
	db.QueryRow(`SELECT COUNT(*) FROM hits_daily`).Scan(&after)
	if before != after {
		t.Errorf("prune deleted %d rows from a ten-week table; it must be a no-op", before-after)
	}
}
