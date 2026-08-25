package main

import (
	"database/sql"
	"github.com/iammatthias/farfield/lib/fleet"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iammatthias/farfield/lib/web"
)

// newTestDB opens a fresh pulse database in t.TempDir at the given filename
// and runs the migrations.
func newTestDB(t *testing.T, dir, name string) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	db := newTestDB(t, t.TempDir(), "pulse.sqlite")
	tmpl, err := web.ParseTemplates(assets, nil)
	if err != nil {
		t.Fatalf("web.ParseTemplates: %v", err)
	}
	return &Server{
		db:   db,
		auth: &web.Auth{DB: db, Password: "secret"},
		rd:   &web.Renderer{Templates: tmpl, AssetVer: "test"},
	}
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// TestSchemaSelfMigrates opens the same database file twice — every
// migration step must be idempotent.
func TestSchemaSelfMigrates(t *testing.T) {
	dir := t.TempDir()
	db1, err := openDB(filepath.Join(dir, "pulse.sqlite"))
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	db1.Close()
	db2, err := openDB(filepath.Join(dir, "pulse.sqlite"))
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	db2.Close()
}

// stubTarget spins up a stub HTTP server whose status is swapped via the
// returned atomic, plus a registered target pointing at it.
func stubTarget(t *testing.T, db *sql.DB) (*Target, *atomic.Int64) {
	t.Helper()
	var status atomic.Int64
	status.Store(200)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(stub.Close)

	target := &Target{Name: "stub", URL: stub.URL, Method: "GET",
		ExpectedStatus: 200, IntervalS: 60, Enabled: true}
	if err := insertTarget(db, target); err != nil {
		t.Fatalf("insertTarget: %v", err)
	}
	return target, &status
}

// TestCheckerIncidentTransitions drives ok→fail→fail→ok at threshold 1 (the
// PULSE_FAIL_THRESHOLD=1 / original behavior) and asserts the incident state
// machine: exactly one incident opens on the first failure, consecutive
// failures update its last_err, and recovery closes it.
func TestCheckerIncidentTransitions(t *testing.T) {
	db := newTestDB(t, t.TempDir(), "pulse.sqlite")
	target, status := stubTarget(t, db)
	client := &http.Client{Timeout: 2 * time.Second}
	probe := func() {
		t.Helper()
		if err := recordCheck(db, target.ID, performCheck(client, *target), 1); err != nil {
			t.Fatalf("recordCheck: %v", err)
		}
	}

	probe() // ok
	if n := countRows(t, db, `SELECT COUNT(*) FROM checks`); n != 1 {
		t.Fatalf("checks = %d, want 1", n)
	}
	var latency int64
	if err := db.QueryRow(`SELECT latency_ms FROM checks`).Scan(&latency); err != nil || latency < 0 {
		t.Fatalf("latency_ms = %d, err %v", latency, err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents`); n != 0 {
		t.Fatalf("incidents after ok = %d, want 0", n)
	}

	status.Store(503)
	probe() // ok → fail: at threshold 1, opens exactly one incident
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents WHERE closed_at = ''`); n != 1 {
		t.Fatalf("open incidents after first fail = %d, want 1", n)
	}

	probe() // fail → fail: still one incident, last_err refreshed
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents`); n != 1 {
		t.Fatalf("incidents after second fail = %d, want 1", n)
	}
	inc, err := openIncident(db, target.ID)
	if err != nil || inc == nil {
		t.Fatalf("openIncident: %v, %v", inc, err)
	}

	status.Store(200)
	probe() // fail → ok: closes it
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents WHERE closed_at = ''`); n != 0 {
		t.Fatalf("open incidents after recovery = %d, want 0", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents`); n != 1 {
		t.Fatalf("total incidents = %d, want 1", n)
	}

	pct, ok, err := uptime(db, target.ID, 24*time.Hour)
	if err != nil || !ok {
		t.Fatalf("uptime: ok=%v err=%v", ok, err)
	}
	if pct != 50 { // 2 of 4 checks ok
		t.Fatalf("uptime = %.2f, want 50.00", pct)
	}
}

// TestIncidentDebounce exercises the default threshold of 2: a single flaked
// check never opens an incident, two consecutive fails open exactly one, its
// last_err keeps updating while open, and the first ok closes it. The flaked
// and failed checks are still recorded, so uptime reflects them.
func TestIncidentDebounce(t *testing.T) {
	db := newTestDB(t, t.TempDir(), "pulse.sqlite")
	target, status := stubTarget(t, db)
	client := &http.Client{Timeout: 2 * time.Second}
	probe := func() {
		t.Helper()
		if err := recordCheck(db, target.ID, performCheck(client, *target), 2); err != nil {
			t.Fatalf("recordCheck: %v", err)
		}
	}

	// Single flake: fail then ok — recorded, but no incident.
	probe() // ok
	status.Store(503)
	probe() // fail #1: below threshold
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents`); n != 0 {
		t.Fatalf("incidents after single fail = %d, want 0", n)
	}
	status.Store(200)
	probe() // ok again: streak reset, still nothing
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents`); n != 0 {
		t.Fatalf("incidents after flake recovery = %d, want 0", n)
	}

	// Real outage: two consecutive fails open exactly one incident.
	status.Store(503)
	probe() // fail #1
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents`); n != 0 {
		t.Fatalf("incidents one fail into outage = %d, want 0", n)
	}
	status.Store(500)
	probe() // fail #2: opens
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents WHERE closed_at = ''`); n != 1 {
		t.Fatalf("open incidents after two fails = %d, want 1", n)
	}

	probe() // fail #3: same incident, no new row
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents`); n != 1 {
		t.Fatalf("incidents after third fail = %d, want 1", n)
	}

	status.Store(200)
	probe() // first ok closes it, undebounced
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents WHERE closed_at = ''`); n != 0 {
		t.Fatalf("open incidents after recovery = %d, want 0", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents`); n != 1 {
		t.Fatalf("total incidents = %d, want 1", n)
	}

	// All seven checks recorded honestly: 3 ok, 4 fail.
	if n := countRows(t, db, `SELECT COUNT(*) FROM checks`); n != 7 {
		t.Fatalf("checks = %d, want 7", n)
	}
	pct, ok, err := uptime(db, target.ID, 24*time.Hour)
	if err != nil || !ok {
		t.Fatalf("uptime: ok=%v err=%v", ok, err)
	}
	if want := 100 * 3.0 / 7.0; pct < want-0.01 || pct > want+0.01 {
		t.Fatalf("uptime = %.2f, want %.2f", pct, want)
	}
}

// TestDebouncedIncidentLastErr: while below threshold no incident exists to
// refresh, and once open the incident's last_err tracks the newest failure.
func TestDebouncedIncidentLastErr(t *testing.T) {
	db := newTestDB(t, t.TempDir(), "pulse.sqlite")
	target := &Target{Name: "synthetic", URL: "http://127.0.0.1:1/x", Method: "GET",
		ExpectedStatus: 200, IntervalS: 60, Enabled: true}
	if err := insertTarget(db, target); err != nil {
		t.Fatalf("insertTarget: %v", err)
	}
	fail := func(msg string) {
		t.Helper()
		if err := recordCheck(db, target.ID, checkResult{Err: msg}, 2); err != nil {
			t.Fatalf("recordCheck: %v", err)
		}
	}

	fail("first miss")
	fail("second miss") // opens with this err
	fail("third miss")  // refreshes last_err
	inc, err := openIncident(db, target.ID)
	if err != nil || inc == nil {
		t.Fatalf("openIncident: %v, %v", inc, err)
	}
	if inc.LastErr != "third miss" {
		t.Fatalf("last_err = %q, want %q", inc.LastErr, "third miss")
	}
	if inc.OpenedAt == "" {
		t.Fatal("opened_at empty")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents`); n != 1 {
		t.Fatalf("incidents = %d, want 1", n)
	}
}

// TestFailThresholdEnv: PULSE_FAIL_THRESHOLD honors explicit values
// (1 restores the original behavior) and falls back to the default of 2
// when unset or nonsense.
func TestFailThresholdEnv(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"", 2}, {"1", 1}, {"3", 3}, {"0", 2}, {"-1", 2}, {"two", 2},
	} {
		t.Setenv("PULSE_FAIL_THRESHOLD", tc.raw)
		if got := failThreshold(); got != tc.want {
			t.Errorf("failThreshold(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// TestProbeRetry: a probe that fails once and succeeds on the in-probe retry
// is recorded as a single ok check — no fail row, no incident. A probe that
// fails twice is recorded as one fail.
func TestProbeRetry(t *testing.T) {
	saved := retryDelay
	retryDelay = 10 * time.Millisecond
	t.Cleanup(func() { retryDelay = saved })

	db := newTestDB(t, t.TempDir(), "pulse.sqlite")
	var calls atomic.Int64
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503) // flake on the first hit only
			return
		}
		w.WriteHeader(200)
	}))
	defer stub.Close()

	target := &Target{Name: "flaky", URL: stub.URL, Method: "GET",
		ExpectedStatus: 200, IntervalS: 60, Enabled: true}
	if err := insertTarget(db, target); err != nil {
		t.Fatalf("insertTarget: %v", err)
	}
	client := &http.Client{Timeout: 2 * time.Second}

	res := probeTarget(client, *target)
	if !res.OK {
		t.Fatalf("probeTarget after flake: ok=false (status %d, err %q)", res.StatusCode, res.Err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("stub hits = %d, want 2 (fail + retry)", n)
	}
	if err := recordCheck(db, target.ID, res, 2); err != nil {
		t.Fatalf("recordCheck: %v", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM checks WHERE ok = 1`); n != 1 {
		t.Fatalf("ok checks = %d, want 1", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM checks WHERE ok = 0`); n != 0 {
		t.Fatalf("failed checks = %d, want 0", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents`); n != 0 {
		t.Fatalf("incidents = %d, want 0", n)
	}

	// Persistent failure: both attempts miss → one recorded fail.
	dead := *target
	dead.URL = "http://127.0.0.1:1/x"
	calls.Store(0)
	res = probeTarget(client, dead)
	if res.OK || res.Err == "" {
		t.Fatalf("probeTarget on dead target: ok=%v err=%q", res.OK, res.Err)
	}
}

// seedSourceDB creates an app's telemetry sidecar (pulse/<app>.sqlite, the
// layout lib/pulse writes) with lib/pulse-shaped request rows and returns it
// for appending more.
func seedSourceDB(t *testing.T, dir, app string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "pulse"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := sql.Open("sqlite",
		"file:"+filepath.Join(dir, "pulse", app+".sqlite")+
			"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	t.Cleanup(func() { src.Close() })
	if _, err := src.Exec(`CREATE TABLE requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL,
		path TEXT NOT NULL, method TEXT NOT NULL, status INTEGER NOT NULL,
		latency_ms INTEGER NOT NULL, vkey TEXT NOT NULL,
		ref_host TEXT NOT NULL DEFAULT '', country TEXT NOT NULL DEFAULT '',
		bot INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("create requests: %v", err)
	}
	return src
}

func addRequest(t *testing.T, src *sql.DB, ts, path, vkey, refHost string) {
	t.Helper()
	if _, err := src.Exec(`INSERT INTO requests
		(ts, path, method, status, latency_ms, vkey, ref_host, country)
		VALUES (?, ?, 'GET', 200, 5, ?, ?, '')`, ts, path, vkey, refHost); err != nil {
		t.Fatalf("insert request: %v", err)
	}
}

// TestCollectorCursor runs the collector twice over the same source rows —
// counts must not double — then appends new rows and checks they roll up.
func TestCollectorCursor(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pulse.sqlite")
	db := newTestDB(t, dir, "pulse.sqlite")
	src := seedSourceDB(t, dir, "blog")

	ts := time.Now().UTC().Format(time.RFC3339)
	addRequest(t, src, ts, "/a", "v1", "example.org")
	addRequest(t, src, ts, "/a", "v2", "")
	addRequest(t, src, ts, "/b", "v1", "example.org")

	collectAll(db, dbPath)
	collectAll(db, dbPath) // same rows again — the cursor must hold

	if hits := countRows(t, db, `SELECT COALESCE(SUM(hits),0) FROM hits_daily`); hits != 3 {
		t.Fatalf("hits after double run = %d, want 3", hits)
	}
	if u := countRows(t, db,
		`SELECT COALESCE(SUM(uniques),0) FROM hits_daily WHERE path = '/a'`); u != 2 {
		t.Fatalf("uniques for /a = %d, want 2", u)
	}
	if refHits := countRows(t, db,
		`SELECT COALESCE(SUM(hits),0) FROM referrers_daily WHERE referrer_host = 'example.org'`); refHits != 2 {
		t.Fatalf("referrer hits = %d, want 2", refHits)
	}

	// New rows after the cursor roll up; a repeated vkey is not re-counted.
	addRequest(t, src, ts, "/a", "v1", "")
	addRequest(t, src, ts, "/a", "v3", "")
	collectAll(db, dbPath)

	if hits := countRows(t, db, `SELECT COALESCE(SUM(hits),0) FROM hits_daily`); hits != 5 {
		t.Fatalf("hits after new rows = %d, want 5", hits)
	}
	if u := countRows(t, db,
		`SELECT COALESCE(SUM(uniques),0) FROM hits_daily WHERE path = '/a'`); u != 3 {
		t.Fatalf("uniques for /a after new rows = %d, want 3 (v1 re-counted?)", u)
	}

	var cursor int64
	if err := db.QueryRow(`SELECT last_event_id FROM collector_cursor
		WHERE app = 'blog'`).Scan(&cursor); err != nil || cursor != 5 {
		t.Fatalf("cursor = %d (err %v), want 5", cursor, err)
	}

	// Pulse's own database must never be collected as an app.
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM collector_cursor WHERE app = 'pulse'`); n != 0 {
		t.Fatal("collector swept pulse's own database")
	}
}

// TestCollectorSkipsAppsWithoutRequests: a stray file in the sidecar
// directory without the lib/pulse table is skipped silently.
func TestCollectorSkipsAppsWithoutRequests(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, dir, "pulse.sqlite")
	if err := os.MkdirAll(filepath.Join(dir, "pulse"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "pulse", "plain.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	plain.Close()

	collectAll(db, filepath.Join(dir, "pulse.sqlite"))
	if n := countRows(t, db, `SELECT COUNT(*) FROM collector_cursor`); n != 0 {
		t.Fatalf("cursor rows = %d, want 0", n)
	}
}

// TestCollectorCursorRewind: a request log rebuilt from scratch (the one-time
// migration off in-app tables, or a wiped sidecar) restarts ids at 1 while
// the stored cursor is still high. The rewind guard must reset and collect
// the new rows instead of hiding them behind the stale cursor forever.
func TestCollectorCursorRewind(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pulse.sqlite")
	db := newTestDB(t, dir, "pulse.sqlite")
	src := seedSourceDB(t, dir, "blog")

	ts := time.Now().UTC().Format(time.RFC3339)
	// The stored cursor says the collector has seen through id 100000 — the
	// legacy table's ids. The rebuilt sidecar holds rows 1 and 2.
	if _, err := db.Exec(`INSERT INTO collector_cursor
		(app, last_event_id, last_run) VALUES ('blog', 100000, ?)`, ts); err != nil {
		t.Fatal(err)
	}
	addRequest(t, src, ts, "/a", "v1", "")
	addRequest(t, src, ts, "/b", "v2", "")

	collectAll(db, dbPath)

	if hits := countRows(t, db, `SELECT COALESCE(SUM(hits),0) FROM hits_daily`); hits != 2 {
		t.Fatalf("hits after rewind = %d, want 2", hits)
	}
	var cursor int64
	if err := db.QueryRow(`SELECT last_event_id FROM collector_cursor
		WHERE app = 'blog'`).Scan(&cursor); err != nil || cursor != 2 {
		t.Fatalf("cursor = %d (err %v), want 2", cursor, err)
	}

	// And the guard must not re-collect on the next pass.
	collectAll(db, dbPath)
	if hits := countRows(t, db, `SELECT COALESCE(SUM(hits),0) FROM hits_daily`); hits != 2 {
		t.Fatalf("hits after second pass = %d, want 2 (double-counted)", hits)
	}
}

// TestPruneChecks: probe results older than the retention window go; newer
// ones and all incidents stay.
func TestPruneChecks(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, dir, "pulse.sqlite")
	target := &Target{Name: "t", URL: "http://x", Method: "GET",
		ExpectedStatus: 200, IntervalS: 60, Enabled: true}
	if err := insertTarget(db, target); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-checksRetention - 24*time.Hour).Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)
	for _, ts := range []string{old, fresh} {
		if _, err := db.Exec(`INSERT INTO checks
			(target_id, ts, status_code, latency_ms, ok) VALUES (?, ?, 200, 5, 1)`,
			target.ID, ts); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO incidents (target_id, opened_at, closed_at)
		VALUES (?, ?, ?)`, target.ID, old, old); err != nil {
		t.Fatal(err)
	}

	pruneChecks(db)

	if n := countRows(t, db, `SELECT COUNT(*) FROM checks`); n != 1 {
		t.Fatalf("checks after prune = %d, want 1", n)
	}
	var ts string
	if err := db.QueryRow(`SELECT ts FROM checks`).Scan(&ts); err != nil || ts != fresh {
		t.Fatalf("surviving check ts = %q, want the fresh one", ts)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM incidents`); n != 1 {
		t.Fatal("incidents must never be pruned")
	}
}

// TestUnauthedRedirects: every console page bounces to /login without a
// session; /status stays public.
func TestUnauthedRedirects(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	for _, path := range []string{"/", "/targets", "/traffic", "/api/overview"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("GET %s = %d, want 303", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/login" {
			t.Fatalf("GET %s redirects to %q, want /login", path, loc)
		}
	}

	resp, err := client.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /status = %d, want 200", resp.StatusCode)
	}
}

// TestTargetCRUDViaForms logs in and drives the target form end to end.
func TestTargetCRUDViaForms(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	jar := newCookieClient(t)
	resp, err := jar.PostForm(srv.URL+"/login", url.Values{"password": {"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp, err = jar.PostForm(srv.URL+"/targets", url.Values{
		"name": {"apex"}, "url": {"https://apex.example/status"},
		"method": {"get"}, "expected_status": {"200"},
		"interval_s": {"30"}, "enabled": {"on"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	targets, err := listTargets(s.db)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %v (err %v), want 1", targets, err)
	}
	tg := targets[0]
	if tg.Method != "GET" || tg.IntervalS != 30 || !tg.Enabled {
		t.Fatalf("stored target = %+v", tg)
	}

	// The overview renders with the new target.
	resp, err = jar.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "apex") {
		t.Fatalf("overview status %d, body misses target", resp.StatusCode)
	}
}

func newCookieClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSeedTargets pins the seeding contract: every public fleet service gets
// a target once; an existing target covering the host is adopted rather than
// duplicated; and a target the operator deletes stays deleted across
// restarts — seeding is once-per-name, not idempotent-creation.
func TestSeedTargets(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, dir, "pulse.sqlite")

	// An operator-created target already covers the content host.
	pre := &Target{Name: "content (manual)", URL: "https://content.farfield.systems/",
		Method: "GET", ExpectedStatus: 200, IntervalS: 300, Enabled: true}
	if err := insertTarget(db, pre); err != nil {
		t.Fatal(err)
	}

	seedTargets(db)

	// content was adopted, not duplicated.
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM targets WHERE url LIKE '%content.farfield.systems%'`); n != 1 {
		t.Fatalf("content targets = %d, want 1 (duplicated)", n)
	}
	// Public services got seeded; the tailnet-only backup did not.
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM targets WHERE url LIKE '%pulse.farfield.systems%'`); n != 1 {
		t.Fatalf("pulse target not seeded")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM targets WHERE name = 'backup'`); n != 0 {
		t.Fatal("tailnet-only backup must not get a public probe")
	}

	// The operator deletes a seeded target; a restart must not resurrect it.
	var qrID int64
	if err := db.QueryRow(`SELECT id FROM targets WHERE name = 'qr'`).Scan(&qrID); err != nil {
		t.Fatalf("seeded qr target missing: %v", err)
	}
	if err := deleteTarget(db, qrID); err != nil {
		t.Fatal(err)
	}
	seedTargets(db)
	if n := countRows(t, db, `SELECT COUNT(*) FROM targets WHERE name = 'qr'`); n != 0 {
		t.Fatal("restart resurrected a deleted target")
	}
}

// TestCollectorBucketsBotsAndNotFound: bot rows collapse onto "(bot)" and
// never count as visitors or referrers; non-bot 404s collapse onto
// "(not found)"; ordinary traffic keeps its real path.
func TestCollectorBucketsBotsAndNotFound(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pulse.sqlite")
	db := newTestDB(t, dir, "pulse.sqlite")
	src := seedSourceDB(t, dir, "blog")

	ts := time.Now().UTC().Format(time.RFC3339)
	insert := func(path string, status, bot int, vkey, ref string) {
		t.Helper()
		if _, err := src.Exec(`INSERT INTO requests
			(ts, path, method, status, latency_ms, vkey, ref_host, country, bot)
			VALUES (?, ?, 'GET', ?, 5, ?, ?, '', ?)`,
			ts, path, status, vkey, ref, bot); err != nil {
			t.Fatal(err)
		}
	}
	insert("/real-page", 200, 0, "v1", "example.org") // a visitor
	insert("/wp-admin/setup.php", 404, 0, "v2", "")   // a scanner guess
	insert("/phpmyadmin/index.php", 404, 0, "v2", "") // another guess
	insert("/anything", 200, 1, "v3", "spam.example") // a declared bot

	collectAll(db, dbPath)

	if n := countRows(t, db,
		`SELECT COALESCE(SUM(hits),0) FROM hits_daily WHERE path = '/real-page'`); n != 1 {
		t.Fatalf("real page hits = %d, want 1", n)
	}
	if n := countRows(t, db,
		`SELECT COALESCE(SUM(hits),0) FROM hits_daily WHERE path = '(not found)'`); n != 2 {
		t.Fatalf("(not found) hits = %d, want 2", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM hits_daily
		WHERE path LIKE '/wp-admin%' OR path LIKE '/phpmyadmin%'`); n != 0 {
		t.Fatal("scanner paths minted their own rows")
	}
	if n := countRows(t, db,
		`SELECT COALESCE(SUM(hits),0) FROM hits_daily WHERE path = '(bot)'`); n != 1 {
		t.Fatalf("(bot) hits = %d, want 1", n)
	}
	// The bot contributed no visitor and no referrer.
	if n := countRows(t, db,
		`SELECT COALESCE(SUM(uniques),0) FROM hits_daily WHERE path = '(bot)'`); n != 0 {
		t.Fatal("bot counted as a unique visitor")
	}
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM referrers_daily WHERE referrer_host = 'spam.example'`); n != 0 {
		t.Fatal("bot referrer recorded")
	}
}

// TestSeedDoesNotAdoptApexFromASibling pins the bug that made this an exact
// host comparison. Apex's public host is the bare domain, which is a suffix
// of every sibling host, so a substring test let any one sibling target mark
// the fleet's own front door as covered — permanently, since the name still
// entered the ledger.
func TestSeedDoesNotAdoptApexFromASibling(t *testing.T) {
	db := newTestDB(t, t.TempDir(), "pulse.sqlite")

	// One sibling target exists. Nothing covers apex.
	sibling := &Target{Name: "content", URL: "https://content.farfield.systems/status",
		Method: "GET", ExpectedStatus: 200, IntervalS: 300, Enabled: true}
	if err := insertTarget(db, sibling); err != nil {
		t.Fatal(err)
	}

	seedTargets(db)

	targets, err := listTargets(db)
	if err != nil {
		t.Fatal(err)
	}
	var apex *Target
	for i := range targets {
		if targets[i].Name == "apex" {
			apex = &targets[i]
			break
		}
	}
	if apex == nil {
		t.Fatal("apex was never seeded — a sibling's URL was mistaken for coverage of the front door")
	}
	if !strings.Contains(apex.URL, "farfield.systems/status") {
		t.Errorf("apex target URL = %q, want the apex /status URL", apex.URL)
	}
}

// TestSameHostRejectsLookalikes covers the comparison directly.
func TestSameHostRejectsLookalikes(t *testing.T) {
	for _, tc := range []struct {
		url, host string
		want      bool
	}{
		{"https://farfield.systems/status", "farfield.systems", true},
		{"https://FarField.Systems/status", "farfield.systems", true},
		{"https://content.farfield.systems/status", "farfield.systems", false},
		{"https://notcontent.farfield.systems/", "content.farfield.systems", false},
		{"https://farfield.systems.example.com/", "farfield.systems", false},
		{"https://uptime.example.io/?host=farfield.systems", "farfield.systems", false},
		{"://bad", "farfield.systems", false},
	} {
		if got := sameHost(tc.url, tc.host); got != tc.want {
			t.Errorf("sameHost(%q, %q) = %v, want %v", tc.url, tc.host, got, tc.want)
		}
	}
}

// TestOrphanedFlagsRetiredHostnamesOnly is the guard for the drift that took a
// week to notice: bard left the fleet, its uptime target stayed, and it probed
// a hostname whose DNS was gone until someone read the red row.
//
// Seeding is additive so a target deleted by hand stays deleted — which means
// nothing can remove one automatically either. Flagging is the most that can be
// done safely, so it has to be accurate: a false positive on a deliberate
// target trains the operator to ignore the flag.
func TestOrphanedFlagsRetiredHostnamesOnly(t *testing.T) {
	// A farfield subdomain no service claims: the actual drift.
	for _, u := range []string{
		"https://bard.farfield.systems/status",
		"https://dead-presidents.farfield.systems/status",
		"http://epochs.farfield.systems/status",
	} {
		if !Orphaned(u) {
			t.Errorf("Orphaned(%q) = false, want true", u)
		}
	}

	// Every service the registry still serves.
	for _, svc := range fleet.Services() {
		if svc.Public == "" {
			continue
		}
		if Orphaned(svc.PublicURL() + "status") {
			t.Errorf("Orphaned flagged %s, which is in the registry", svc.Name)
		}
	}

	// Deliberate targets that are not farfield hostnames at all. Flagging these
	// is the failure mode that would make the badge worthless.
	for _, u := range []string{
		"http://backup:8791/status",         // compose name, internal by design
		"http://scrap:8799/status",          // ditto
		"http://172.17.0.1:8802/status",     // the host gateway
		"https://bard.pure---internet.com/", // another project entirely
		"https://example.com/health",        // somebody else's site
		"not a url at all",
	} {
		if Orphaned(u) {
			t.Errorf("Orphaned(%q) = true, want false", u)
		}
	}

	// A hostname that merely CONTAINS the domain is not under it.
	if Orphaned("https://farfield.systems.evil.example/status") {
		t.Error("Orphaned matched a lookalike host by substring")
	}
}
