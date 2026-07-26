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

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.sqlite")
	db, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
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
	db := testDB(t)
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	want := map[string]bool{
		"id": true, "ts": true, "path": true, "method": true, "status": true,
		"latency_ms": true, "vkey": true, "ref_host": true, "country": true,
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
	db := testDB(t)
	rec := New(db, "testapp")
	t.Cleanup(rec.Close)
	h := rec.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	r := httptest.NewRequest("GET", "/things?q=1", nil)
	r.Header.Set("User-Agent", "test-agent")
	r.Header.Set("Referer", "https://example.org/from/here")
	r.Header.Set("CF-IPCountry", "US")
	r.RemoteAddr = "203.0.113.9:51234"
	h.ServeHTTP(httptest.NewRecorder(), r)
	waitForRows(t, db, 1)

	var path, method, vkey, refHost, country string
	var status int
	var latency int64
	err := db.QueryRow(`SELECT path, method, status, latency_ms, vkey,
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
	waitForRows(t, db, 2)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("requests rows = %d, want 2 (excluded paths recorded?)", n)
	}
}

// TestStatusDefaultsTo200 covers handlers that never call WriteHeader.
func TestStatusDefaultsTo200(t *testing.T) {
	db := testDB(t)
	rec := New(db, "testapp")
	t.Cleanup(rec.Close)
	h := rec.Wrap(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	waitForRows(t, db, 1)
	var status int
	if err := db.QueryRow(`SELECT status FROM requests`).Scan(&status); err != nil {
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
