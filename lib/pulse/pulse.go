// Package pulse provides a privacy-preserving traffic-recording middleware
// for farfield apps. Each handled request becomes one row in a `requests`
// table inside a telemetry sidecar database — `pulse/<app>.sqlite` beside the
// app's own database, never in the app's database itself; the pulse app's
// collector later reads those rows (read-only, by cursor) and rolls them up
// into daily aggregates.
//
// The sidecar exists for the backup pipeline's sake. Request rows are rolling
// two-week telemetry, yet when they lived in the app's database they dirtied
// it on every hit, so every 6-hour snapshot of every app hashed to a new CID
// and uploaded a full copy — the backup app's content-addressed dedup could
// never fire. With telemetry off to the side, an app whose real data has not
// changed snapshots to an identical CID and uploads nothing.
//
// Privacy by construction: no raw IP and no raw User-Agent are ever stored.
// A request is attributed to a visitor key (vkey) — the first 8 bytes, hex,
// of sha256(daySalt + clientIP + userAgent). The day salt is 32 bytes from
// crypto/rand held only in memory and regenerated when the UTC date changes,
// so vkeys cannot be linked across days even with the database in hand. (A
// process restart mid-day regenerates the salt, which slightly inflates that
// day's unique count — an accepted trade-off for never persisting the salt.)
//
// Excluded from recording: GET /status (the Docker healthcheck probe) and
// anything under /static/ (asset noise) — both would swamp the table with
// rows that say nothing about real traffic.
//
// The package depends only on the standard library plus lib/web, for
// web.ClientIP — resolving the client address is a security-sensitive
// decision (forwarded headers are only believed from a trusted peer), and one
// copy of that rule is the only safe number. The importing app registers the
// SQLite driver, exactly like lib/store.
package pulse

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iammatthias/farfield/lib/web"
)

// schema is the requests table the middleware self-creates on first use.
// The INTEGER PRIMARY KEY is SQLite's rowid, which is itself the table's
// b-tree key — the collector's `WHERE id > ? ORDER BY id` scan needs no
// further index.
const schema = `
CREATE TABLE IF NOT EXISTS requests (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	ts         TEXT    NOT NULL,
	path       TEXT    NOT NULL,
	method     TEXT    NOT NULL,
	status     INTEGER NOT NULL,
	latency_ms INTEGER NOT NULL,
	vkey       TEXT    NOT NULL,
	ref_host   TEXT    NOT NULL DEFAULT '',
	country    TEXT    NOT NULL DEFAULT ''
);`

// retention is how long raw request rows are kept before the prune goroutine
// deletes them. The pulse collector aggregates rows within minutes, so two
// weeks is generous headroom.
const retention = 14 * 24 * time.Hour

// chanSize bounds the write queue. When the writer goroutine cannot keep up,
// new events are dropped — the request path never blocks on SQLite.
const chanSize = 256

// New creates a traffic Recorder for the named app: it opens the telemetry
// sidecar database (`pulse/<name>.sqlite` beside the app's database, derived
// from the handle itself via PRAGMA database_list), self-creates the requests
// table there, then starts a single writer goroutine (so request handling
// never blocks on SQLite) and a daily prune goroutine. Wrap a handler with the
// returned recorder to record one row per request; call Close on shutdown to
// stop the goroutines and flush the queue. If the sidecar cannot be opened it
// logs and returns nil — a nil Recorder's Wrap is a pass-through; recording is
// disabled rather than falling back to writing into the app's database, which
// would silently re-dirty its backup snapshots.
//
// A legacy `requests` table from the versions that wrote into the app's own
// database is dropped once the sidecar is ready — its rows are rolling
// telemetry the collector has already aggregated, and leaving them would keep
// a stale copy in every future snapshot.
func New(db *sql.DB, app string) *Recorder {
	tdb, err := openSidecar(db)
	if err != nil {
		slog.Error("pulse: recording disabled, could not open telemetry sidecar",
			"app", app, "err", err)
		return nil
	}
	dropLegacyTable(db, app)
	rec := &Recorder{
		db:   tdb,
		app:  app,
		ch:   make(chan event, chanSize),
		salt: newSalter(time.Now),
		quit: make(chan struct{}),
	}
	rec.wg.Add(2)
	go rec.writeLoop()
	go rec.pruneLoop()
	return rec
}

// openSidecar locates the app's database file through the handle itself
// (PRAGMA database_list, so no app has to pass its path twice), then opens
// the sidecar at pulse/<same-filename> in the same directory, creating the
// directory and the requests table as needed. One connection is enough: the
// writer goroutine and the daily prune are the only users.
func openSidecar(db *sql.DB) (*sql.DB, error) {
	appPath, err := mainDBFile(db)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(filepath.Dir(appPath), "pulse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, filepath.Base(appPath))
	tdb, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"+
			"&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	tdb.SetMaxOpenConns(1)
	tdb.SetMaxIdleConns(1)
	if _, err := tdb.Exec(schema); err != nil {
		tdb.Close()
		return nil, err
	}
	return tdb, nil
}

// mainDBFile returns the file backing the handle's `main` database. An
// in-memory or otherwise fileless database is an error — there is nowhere to
// put a sidecar next to it.
func mainDBFile(db *sql.DB) (string, error) {
	rows, err := db.Query(`PRAGMA database_list`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", err
		}
		if name == "main" && file != "" {
			return file, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "", errors.New("app database is not file-backed")
}

// dropLegacyTable removes the requests table that earlier versions created in
// the app's own database. Only a table carrying lib/pulse's vkey column is
// touched — a future app with its own domain `requests` table keeps it.
func dropLegacyTable(db *sql.DB, app string) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('requests')
		WHERE name = 'vkey'`).Scan(&n)
	if err != nil || n == 0 {
		return
	}
	if _, err := db.Exec(`DROP TABLE requests`); err != nil {
		slog.Warn("pulse: could not drop legacy requests table", "app", app, "err", err)
	}
}

// Wrap returns next wrapped with request recording. A nil recorder returns next
// unchanged, so an app (or a test) that never starts pulse is a clean
// pass-through rather than a special case.
func (rec *Recorder) Wrap(next http.Handler) http.Handler {
	if rec == nil {
		return next
	}
	return rec.middleware(next)
}

// Close stops the writer and prune goroutines, flushing whatever is already
// queued, then closes the sidecar database. Safe on a nil recorder. Apps call
// it on shutdown, once the HTTP server has stopped accepting requests, so no
// send races the drain.
func (rec *Recorder) Close() {
	if rec == nil {
		return
	}
	close(rec.quit)
	rec.wg.Wait()
	rec.db.Close()
}

// event is one recorded request, queued for the writer goroutine.
type event struct {
	ts        string
	path      string
	method    string
	status    int
	latencyMS int64
	vkey      string
	refHost   string
	country   string
}

// Recorder owns the sidecar database handle, the write queue, the day salt,
// and drop accounting. Its goroutines run until Close, which stops them,
// flushes the queue, and closes the handle.
type Recorder struct {
	db   *sql.DB // the telemetry sidecar, owned by the recorder — never the app's database
	app  string
	ch   chan event
	salt *salter
	quit chan struct{}
	wg   sync.WaitGroup

	drops    atomic.Int64 // events dropped since the last warning
	lastWarn atomic.Int64 // unix time of the last drop warning
}

// skip reports whether a request path is excluded from recording: the
// /status healthcheck probe and static assets.
func skip(path string) bool {
	return path == "/status" || strings.HasPrefix(path, "/static/")
}

func (rec *Recorder) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)

		ev := event{
			ts:        start.UTC().Format(time.RFC3339),
			path:      r.URL.Path,
			method:    r.Method,
			status:    sw.code(),
			latencyMS: time.Since(start).Milliseconds(),
			vkey:      rec.salt.vkey(web.ClientIP(r), r.UserAgent()),
			refHost:   refHost(r.Referer()),
			country:   r.Header.Get("CF-IPCountry"),
		}
		select {
		case rec.ch <- ev:
		default:
			rec.noteDrop()
		}
	})
}

// writeLoop is the single writer: it drains the event channel into the requests
// table until Close signals quit, then flushes whatever is still queued and
// exits. One goroutine per recorder.
func (rec *Recorder) writeLoop() {
	defer rec.wg.Done()
	for {
		select {
		case ev := <-rec.ch:
			rec.write(ev)
		case <-rec.quit:
			for { // flush the backlog so a clean shutdown loses nothing buffered
				select {
				case ev := <-rec.ch:
					rec.write(ev)
				default:
					return
				}
			}
		}
	}
}

// write inserts one recorded event, counting a drop on failure.
func (rec *Recorder) write(ev event) {
	_, err := rec.db.Exec(`INSERT INTO requests
		(ts, path, method, status, latency_ms, vkey, ref_host, country)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ts, ev.path, ev.method, ev.status, ev.latencyMS,
		ev.vkey, ev.refHost, ev.country)
	if err != nil {
		rec.noteDrop()
	}
}

// pruneLoop deletes requests older than the retention window, once at startup
// and once a day after, until Close. RFC 3339 UTC timestamps compare lexically,
// so a plain string comparison is a correct time comparison.
func (rec *Recorder) pruneLoop() {
	defer rec.wg.Done()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	rec.prune()
	for {
		select {
		case <-rec.quit:
			return
		case <-ticker.C:
			rec.prune()
		}
	}
}

func (rec *Recorder) prune() {
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339)
	if _, err := rec.db.Exec(`DELETE FROM requests WHERE ts < ?`, cutoff); err != nil {
		slog.Warn("pulse: prune failed", "app", rec.app, "err", err)
	}
}

// noteDrop counts a dropped (or failed) event and logs at most once a minute
// so a sustained overflow cannot flood the logs.
func (rec *Recorder) noteDrop() {
	n := rec.drops.Add(1)
	now := time.Now().Unix()
	last := rec.lastWarn.Load()
	if now-last >= 60 && rec.lastWarn.CompareAndSwap(last, now) {
		slog.Warn("pulse: dropping request events", "app", rec.app, "dropped", n)
		rec.drops.Add(-n)
	}
}

// ── response status capture ────────────────────────────────────────────────

// statusWriter records the response status code. It forwards Flush so
// streaming handlers keep working through the wrapper.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	if sw.status == 0 {
		sw.status = code
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK
	}
	return sw.ResponseWriter.Write(b)
}

func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom keeps the kernel's sendfile path reachable. This wrapper sits
// innermost — closest to the handler — so http.ServeContent's copy lands here
// first; without it, every byte of an EPUB or a video download would be
// dragged through userspace.
func (sw *statusWriter) ReadFrom(src io.Reader) (int64, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK
	}
	if rf, ok := sw.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	// Copy through Write, not through this type, or ReadFrom recurses.
	return io.Copy(struct{ io.Writer }{sw.ResponseWriter}, src)
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (sw *statusWriter) Unwrap() http.ResponseWriter { return sw.ResponseWriter }

// code returns the captured status, defaulting to 200 for handlers that
// never call WriteHeader.
func (sw *statusWriter) code() int {
	if sw.status == 0 {
		return http.StatusOK
	}
	return sw.status
}

// ── visitor key ────────────────────────────────────────────────────────────

// salter produces visitor keys from a per-UTC-day random salt that lives only
// in memory. The clock is injectable so tests can force a day boundary.
type salter struct {
	mu   sync.Mutex
	now  func() time.Time
	day  string // "2006-01-02" UTC of the current salt
	salt []byte
}

func newSalter(now func() time.Time) *salter {
	return &salter{now: now}
}

// vkey returns the visitor key for a client: the first 8 bytes, hex-encoded,
// of sha256(daySalt + clientIP + userAgent). The salt is regenerated when
// the UTC date has changed since the last use, so keys from different days
// are unlinkable. The raw IP and UA exist only as hash input, never stored.
func (s *salter) vkey(ip, ua string) string {
	s.mu.Lock()
	day := s.now().UTC().Format("2006-01-02")
	if day != s.day {
		s.day = day
		s.salt = make([]byte, 32)
		_, _ = rand.Read(s.salt)
	}
	h := sha256.New()
	h.Write(s.salt)
	s.mu.Unlock()
	h.Write([]byte(ip))
	h.Write([]byte(ua))
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// refHost reduces a Referer header to its host — the only part recorded.
func refHost(referer string) string {
	if referer == "" {
		return ""
	}
	u, err := url.Parse(referer)
	if err != nil {
		return ""
	}
	return u.Host
}
