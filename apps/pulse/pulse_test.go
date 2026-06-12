package main

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
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

// seedSourceDB creates a sibling app database with lib/pulse-shaped request
// rows and returns it for appending more.
func seedSourceDB(t *testing.T, dir, app string) *sql.DB {
	t.Helper()
	src, err := sql.Open("sqlite",
		"file:"+filepath.Join(dir, app+".sqlite")+
			"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	t.Cleanup(func() { src.Close() })
	if _, err := src.Exec(`CREATE TABLE requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL,
		path TEXT NOT NULL, method TEXT NOT NULL, status INTEGER NOT NULL,
		latency_ms INTEGER NOT NULL, vkey TEXT NOT NULL,
		ref_host TEXT NOT NULL DEFAULT '', country TEXT NOT NULL DEFAULT '')`); err != nil {
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

// TestCollectorSkipsAppsWithoutRequests: a sibling db without the lib/pulse
// table is skipped silently.
func TestCollectorSkipsAppsWithoutRequests(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t, dir, "pulse.sqlite")
	plain, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "plain.sqlite"))
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
