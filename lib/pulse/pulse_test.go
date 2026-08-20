package pulse

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // test-only: the library itself is stdlib-only

	"github.com/iammatthias/farfield/lib/web"
)

// testDB creates a file-backed app database in its own temp dir and returns
// it along with the path where New will put the telemetry sidecar.
func testDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.sqlite")
	db, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, filepath.Join(dir, "pulse", "app.sqlite")
}

// readSidecar opens a second handle on the sidecar New created, for
// assertions — the recorder owns and closes its own.
func readSidecar(t *testing.T, path string) *sql.DB {
	t.Helper()
	sc, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open sidecar: %v", err)
	}
	t.Cleanup(func() { sc.Close() })
	return sc
}

// TestVKeyRotatesAcrossDays drives the salter's injectable clock across a UTC
// midnight: the same client must get a different vkey on the new day, and a
// stable one within a day.
func TestVKeyRotatesAcrossDays(t *testing.T) {
	now := time.Date(2026, 6, 10, 23, 59, 0, 0, time.UTC)
	s := newSalter(func() time.Time { return now })

	k1 := s.vkey("203.0.113.7", "Mozilla/5.0")
	k1again := s.vkey("203.0.113.7", "Mozilla/5.0")
	if k1 != k1again {
		t.Fatalf("vkey unstable within a day: %q vs %q", k1, k1again)
	}
	if other := s.vkey("203.0.113.8", "Mozilla/5.0"); other == k1 {
		t.Fatal("different clients share a vkey")
	}

	now = now.Add(2 * time.Minute) // crosses UTC midnight
	k2 := s.vkey("203.0.113.7", "Mozilla/5.0")
	if k2 == k1 {
		t.Fatal("vkey did not rotate across the UTC day boundary")
	}
	if len(k2) != 16 {
		t.Fatalf("vkey length = %d, want 16 hex chars", len(k2))
	}
}

// TestNoRawIPOrUAPersisted asserts the schema by columns: the requests table
// must hold exactly the declared privacy-safe set — no IP, no user agent.
func TestNoRawIPOrUAPersisted(t *testing.T) {
	db, _ := testDB(t)
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	want := map[string]bool{
		"id": true, "ts": true, "path": true, "method": true, "status": true,
		"latency_ms": true, "vkey": true, "ref_host": true, "country": true,
		"bot": true, // a classification — never the User-Agent itself
	}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('requests')`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	got := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if !want[name] {
			t.Fatalf("unexpected column %q in requests — raw client data must not be persisted", name)
		}
		got++
	}
	if got != len(want) {
		t.Fatalf("requests has %d columns, want %d", got, len(want))
	}
}

// waitForRows polls until the async writer has landed n rows or the deadline
// passes.
func waitForRows(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&got); err == nil && got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("writer did not land %d rows in time", n)
}

// TestMiddlewareRecordsRow drives a wrapped handler via httptest and checks
// the recorded row — and that excluded paths record nothing.
func TestMiddlewareRecordsRow(t *testing.T) {
	db, scPath := testDB(t)
	rec := New(db, "testapp")
	t.Cleanup(rec.Close)
	sc := readSidecar(t, scPath)
	h := rec.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	r := httptest.NewRequest("GET", "/things?q=1", nil)
	r.Header.Set("User-Agent", "test-agent")
	r.Header.Set("Referer", "https://example.org/from/here")
	r.Header.Set("CF-IPCountry", "US")
	r.RemoteAddr = "203.0.113.9:51234"
	h.ServeHTTP(httptest.NewRecorder(), r)
	waitForRows(t, sc, 1)

	var path, method, vkey, refHost, country string
	var status int
	var latency int64
	err := sc.QueryRow(`SELECT path, method, status, latency_ms, vkey,
		ref_host, country FROM requests`).
		Scan(&path, &method, &status, &latency, &vkey, &refHost, &country)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if path != "/things" || method != "GET" || status != http.StatusTeapot {
		t.Fatalf("row = %s %s %d, want GET /things 418", method, path, status)
	}
	if refHost != "example.org" {
		t.Fatalf("ref_host = %q, want example.org", refHost)
	}
	if country != "US" {
		t.Fatalf("country = %q, want US", country)
	}
	if len(vkey) != 16 || vkey == "203.0.113.9" {
		t.Fatalf("vkey = %q, want 16 hex chars and never the raw IP", vkey)
	}
	if latency < 0 {
		t.Fatalf("latency_ms = %d", latency)
	}

	// Excluded paths must not be recorded.
	for _, p := range []string{"/status", "/static/styles.css"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/other", nil))
	waitForRows(t, sc, 2)
	var n int
	if err := sc.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("requests rows = %d, want 2 (excluded paths recorded?)", n)
	}
}

// TestSidecarIsolatesTelemetry pins the structural contract: traffic never
// touches the app's own database (so backup snapshots of it stay
// content-stable), a legacy in-app requests table is dropped, and the app's
// own tables survive the migration.
func TestSidecarIsolatesTelemetry(t *testing.T) {
	db, scPath := testDB(t)
	// An earlier lib/pulse wrote its table into the app database; an app also
	// has domain tables of its own.
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	rec := New(db, "testapp")
	if rec == nil {
		t.Fatal("New returned nil for a file-backed database")
	}
	t.Cleanup(rec.Close)

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'requests'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("legacy requests table survived in the app database")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'notes'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("app's own table lost in migration (n=%d, err=%v)", n, err)
	}

	// Traffic lands in the sidecar and only there.
	h := rec.Wrap(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	waitForRows(t, readSidecar(t, scPath), 1)
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'requests'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("traffic recreated requests in the app database (n=%d, err=%v)", n, err)
	}
}

// TestStatusDefaultsTo200 covers handlers that never call WriteHeader.
func TestStatusDefaultsTo200(t *testing.T) {
	db, scPath := testDB(t)
	rec := New(db, "testapp")
	t.Cleanup(rec.Close)
	sc := readSidecar(t, scPath)
	h := rec.Wrap(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	waitForRows(t, sc, 1)
	var status int
	if err := sc.QueryRow(`SELECT status FROM requests`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
}

// Address resolution now lives in web.ClientIP (one copy of a
// security-sensitive rule), and is tested there. This checks the wiring: the
// recorder attributes a request to the address that helper returns, and a
// forwarded header from an untrusted peer does not move it.
func TestVisitorKeyUsesResolvedClientIP(t *testing.T) {
	s := newSalter(time.Now)

	trusted := httptest.NewRequest("GET", "/", nil)
	trusted.RemoteAddr = "10.0.0.1:1234" // private: a real proxy hop
	trusted.Header.Set("CF-Connecting-IP", "203.0.113.2")

	spoofer := httptest.NewRequest("GET", "/", nil)
	spoofer.RemoteAddr = "198.51.100.9:1234" // public: talking to us directly
	spoofer.Header.Set("CF-Connecting-IP", "203.0.113.2")

	if got, want := web.ClientIP(trusted), "203.0.113.2"; got != want {
		t.Fatalf("behind a proxy: ClientIP = %q, want %q", got, want)
	}
	if got, want := web.ClientIP(spoofer), "198.51.100.9"; got != want {
		t.Fatalf("direct client: ClientIP = %q, want %q (header must be ignored)", got, want)
	}
	// Different resolved addresses must land on different visitor keys.
	if a, b := s.vkey(web.ClientIP(trusted), "UA"), s.vkey(web.ClientIP(spoofer), "UA"); a == b {
		t.Error("a spoofed header collapsed two visitors onto one key")
	}
}

// TestBotClassification: declared clients and missing UAs are bots; real
// browser strings are not — and the row records the verdict.
func TestBotClassification(t *testing.T) {
	for ua, want := range map[string]bool{
		"":           true,
		"curl/8.4.0": true,
		"Googlebot/2.1 (+http://www.google.com/bot.html)": true,
		"python-requests/2.31":                            true,
		"Go-http-client/2.0":                              true,
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36":                       false,
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1": false,
	} {
		if got := isBot(ua); got != want {
			t.Errorf("isBot(%q) = %v, want %v", ua, got, want)
		}
	}

	db, scPath := testDB(t)
	rec := New(db, "testapp")
	t.Cleanup(rec.Close)
	sc := readSidecar(t, scPath)
	h := rec.Wrap(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))

	bot := httptest.NewRequest("GET", "/scan", nil)
	bot.Header.Set("User-Agent", "zgrab/0.x")
	h.ServeHTTP(httptest.NewRecorder(), bot)
	human := httptest.NewRequest("GET", "/page", nil)
	human.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/126.0 Safari/537.36")
	h.ServeHTTP(httptest.NewRecorder(), human)
	waitForRows(t, sc, 2)

	var botFlag int
	if err := sc.QueryRow(`SELECT bot FROM requests WHERE path = '/scan'`).Scan(&botFlag); err != nil || botFlag != 1 {
		t.Fatalf("bot row flag = %d (err %v), want 1", botFlag, err)
	}
	if err := sc.QueryRow(`SELECT bot FROM requests WHERE path = '/page'`).Scan(&botFlag); err != nil || botFlag != 0 {
		t.Fatalf("human row flag = %d (err %v), want 0", botFlag, err)
	}
}
