package main

import (
	"database/sql"
	"errors"

	"github.com/iammatthias/farfield/lib/store"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Message is one inbound iMessage and what switchboard did with it. The row is
// three things at once: the idempotency record (Photon retries, and a webhook
// delivered twice must not post twice), the audit log the console renders, and
// the undo/append target — /undo and /append both work by finding the most
// recent successful row for a sender.
type Message struct {
	ID         string `json:"id"` // Photon's message id — the idempotency key
	WebhookID  string `json:"webhookId"`
	Sender     string `json:"sender"` // E.164, normalized
	ChatGUID   string `json:"chatGuid"`
	Body       string `json:"body"`
	Route      string `json:"route"`  // a command name from the registry, or "none"
	Ref        string `json:"ref"`    // id of whatever was created (slug, bookmark id)
	Reply      string `json:"reply"`  // what we texted back; replayed on a retry
	Status     string `json:"status"` // ok | ignored | error
	ReceivedAt string `json:"receivedAt"`
}

// Status values. `ignored` covers every well-formed message we chose not to act
// on (wrong sender, group thread, empty body) — distinct from `error`, which
// means we tried and the downstream service failed.
const (
	statusOK      = "ok"
	statusIgnored = "ignored"
	statusError   = "error"
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	id          TEXT PRIMARY KEY,
	webhook_id  TEXT NOT NULL DEFAULT '',
	sender      TEXT NOT NULL DEFAULT '',
	chat_guid   TEXT NOT NULL DEFAULT '',
	body        TEXT NOT NULL DEFAULT '',
	route       TEXT NOT NULL DEFAULT '',
	ref         TEXT NOT NULL DEFAULT '',
	reply       TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL DEFAULT '',
	received_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_by_received ON messages (received_at DESC);
CREATE INDEX IF NOT EXISTS messages_by_sender ON messages (sender, received_at DESC);`

const messageCols = `id, webhook_id, sender, chat_guid, body, route, ref, reply, status, received_at`

// openDB opens the SQLite database, applies pragmas, and migrates.
func openDB(path string) (*sql.DB, error) {
	db, err := store.OpenDB(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if _, err := db.Exec(jobSchema); err != nil {
		return nil, err
	}
	if _, err := db.Exec(store.SessionSchema); err != nil {
		return nil, err
	}
	return db, nil
}

func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	var m Message
	if err := row.Scan(&m.ID, &m.WebhookID, &m.Sender, &m.ChatGUID, &m.Body,
		&m.Route, &m.Ref, &m.Reply, &m.Status, &m.ReceivedAt); err != nil {
		return nil, err
	}
	return &m, nil
}

// getMessage returns a recorded message by id, or (nil, nil) if absent. A hit
// means this webhook already ran to completion — the caller replays its reply
// rather than dispatching again.
func getMessage(db *sql.DB, id string) (*Message, error) {
	m, err := scanMessage(db.QueryRow(
		`SELECT `+messageCols+` FROM messages WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// recordMessage writes the outcome of one inbound message.
//
// INSERT OR REPLACE rather than plain INSERT: a delivery that crashed midway
// leaves no row (we only write on completion), but a retry of a message whose
// row exists must not error out — the caller has already checked getMessage and
// decided to act, so the newest outcome is the right one to keep.
func recordMessage(db *sql.DB, m *Message) error {
	if m.ReceivedAt == "" {
		m.ReceivedAt = store.NowRFC3339()
	}
	_, err := db.Exec(
		`INSERT OR REPLACE INTO messages (`+messageCols+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.WebhookID, m.Sender, m.ChatGUID, m.Body,
		m.Route, m.Ref, m.Reply, m.Status, m.ReceivedAt)
	return err
}

// lastAction returns a sender's most recent successful message on one of the
// given routes, or (nil, nil) if there is none. It backs /undo, `+`, and /tags,
// which all mean "the thing I just did".
//
// Scoped to the sender rather than global: the allowlist means there is usually
// one, but "my last post" must never resolve to somebody else's.
func lastAction(db *sql.DB, sender string, routes ...string) (*Message, error) {
	if len(routes) == 0 {
		return nil, nil
	}
	query := `SELECT ` + messageCols + ` FROM messages
	          WHERE sender = ? AND status = ? AND route IN (?`
	args := []any{sender, statusOK, routes[0]}
	for _, r := range routes[1:] {
		query += `, ?`
		args = append(args, r)
	}
	query += `) ORDER BY received_at DESC, rowid DESC LIMIT 1`

	m, err := scanMessage(db.QueryRow(query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// listMessages returns the newest messages for the console.
func listMessages(db *sql.DB, limit int) ([]Message, error) {
	rows, err := db.Query(
		`SELECT `+messageCols+` FROM messages ORDER BY received_at DESC, rowid DESC LIMIT ?`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// countMessages returns the number of recorded messages, for /status.
func countMessages(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	return n, err
}

// pruneMessages drops rows past the retention window. The log is an operational
// aid, not an archive — the records it points at live in the apps that own them
// — so it is bounded from the start rather than growing for the life of the
// deployment.
func pruneMessages(db *sql.DB, cutoff string) error {
	_, err := db.Exec(`DELETE FROM messages WHERE received_at < ?`, cutoff)
	return err
}

// ── jobs ───────────────────────────────────────────────────────────────────

// Job is one agent turn.
//
// It is a row rather than a goroutine's local state because an agent turn can
// outlive the webhook that started it, and sometimes outlives the process: a
// restart mid-turn has to leave evidence rather than a message that silently
// never comes back. It also backs /jobs, /job and /cancel, which are the only
// way to see work that is happening somewhere other than in the thread.
type Job struct {
	ID         string `json:"id"`
	MessageID  string `json:"messageId"`
	ChatGUID   string `json:"chatGuid"`
	Sender     string `json:"sender"`
	Prompt     string `json:"prompt"`
	Status     string `json:"status"`
	Result     string `json:"result"`
	Error      string `json:"error"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
}

// Job status values.
const (
	jobRunning   = "running"
	jobDone      = "done"
	jobFailed    = "failed"
	jobCancelled = "cancelled"
)

const jobSchema = `
CREATE TABLE IF NOT EXISTS jobs (
	id          TEXT PRIMARY KEY,
	message_id  TEXT NOT NULL DEFAULT '',
	chat_guid   TEXT NOT NULL DEFAULT '',
	sender      TEXT NOT NULL DEFAULT '',
	prompt      TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL DEFAULT '',
	result      TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	started_at  TEXT NOT NULL,
	finished_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_by_started ON jobs (started_at DESC);
CREATE INDEX IF NOT EXISTS jobs_by_sender ON jobs (sender, started_at DESC);`

const jobCols = `id, message_id, chat_guid, sender, prompt, status, result, error, started_at, finished_at`

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	if err := row.Scan(&j.ID, &j.MessageID, &j.ChatGUID, &j.Sender, &j.Prompt,
		&j.Status, &j.Result, &j.Error, &j.StartedAt, &j.FinishedAt); err != nil {
		return nil, err
	}
	return &j, nil
}

func insertJob(db *sql.DB, j *Job) error {
	if j.StartedAt == "" {
		j.StartedAt = store.NowRFC3339()
	}
	_, err := db.Exec(`INSERT INTO jobs (`+jobCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.MessageID, j.ChatGUID, j.Sender, j.Prompt,
		j.Status, j.Result, j.Error, j.StartedAt, j.FinishedAt)
	return err
}

// finishJob records a terminal outcome. It refuses to move a job that is
// already finished, so a cancel that lands while the turn is completing cannot
// overwrite the real result with "cancelled".
func finishJob(db *sql.DB, id, status, result, errMsg string) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ?, result = ?, error = ?, finished_at = ?
		 WHERE id = ? AND status = ?`,
		status, result, errMsg, store.NowRFC3339(), id, jobRunning)
	return err
}

func getJob(db *sql.DB, id string) (*Job, error) {
	j, err := scanJob(db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return j, nil
}

// listJobs returns a sender's most recent jobs, newest first.
func listJobs(db *sql.DB, sender string, limit int) ([]Job, error) {
	rows, err := db.Query(`SELECT `+jobCols+` FROM jobs WHERE sender = ?
	                       ORDER BY started_at DESC, rowid DESC LIMIT ?`, sender, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// failOrphanedJobs marks jobs that were running when the process died.
//
// Without it a restart leaves a row that says "running" forever and a sender
// waiting for a message that can never arrive — the agent process died with
// its parent.
func failOrphanedJobs(db *sql.DB) (int64, error) {
	res, err := db.Exec(
		`UPDATE jobs SET status = ?, error = ?, finished_at = ? WHERE status = ?`,
		jobFailed, "switchboard restarted while this was running",
		store.NowRFC3339(), jobRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// runningJobCount bounds concurrency across restarts as well as within a run.
func runningJobCount(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status = ?`, jobRunning).Scan(&n)
	return n, err
}

// pruneJobs drops finished jobs past the retention window. Bounded from the
// start, like the message log: the interesting record is the reply that was
// already sent, not the transcript of how it was produced.
func pruneJobs(db *sql.DB, cutoff string) error {
	_, err := db.Exec(`DELETE FROM jobs WHERE status <> ? AND started_at < ?`, jobRunning, cutoff)
	return err
}
