package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/iammatthias/farfield/lib/cid"
)

// The website is a static build. Publishing here changes nothing at the edge
// until that build runs again, so content pokes a Cloudflare deploy hook when —
// and only when — the surface the build reads has actually changed.
//
// "Actually changed" is the whole design. A draft edit, an autosave, trashing a
// draft, uploading a blob nothing references yet: none of these alter a single
// published byte, and each one would otherwise spend a build. Rather than teach
// twenty-odd handlers which of their outcomes are publicly visible, the trigger
// hashes the public surface before and after each mutating request and fires on
// difference. A route added later is covered without being told about this.
const (
	// rebuildDebounce is how long after the last qualifying write the hook
	// fires. A sync-vault run pushes the whole vault's pending edits at once,
	// so the window has to outlast a batch, not a keystroke.
	rebuildDebounce = 30 * time.Second

	// rebuildMaxWait caps how long writes can keep deferring the fire. Without
	// it a long editing session — each save resetting the timer — would defer
	// publication indefinitely.
	rebuildMaxWait = 5 * time.Minute

	// rebuildPostTimeout bounds the hook POST. The hook only queues a build;
	// it does not wait for one, so this is generous.
	rebuildPostTimeout = 15 * time.Second
)

// rebuildTrigger coalesces a burst of writes into a single deploy-hook POST.
//
// It is deliberately stateless about builds. Cloudflare dedupes a hook that
// arrives while a build is queued-but-unstarted, and a hook that arrives after
// a build started correctly queues a second one — that build snapshotted the
// content before the write, so a second is genuinely needed. Tracking build
// state here could only get that wrong.
type rebuildTrigger struct {
	// post sends one hook request. Injected so tests never reach the network.
	post func(context.Context) error

	mu      sync.Mutex
	timer   *time.Timer
	pending bool
	first   time.Time // when the current pending window opened
	closed  bool
}

// newRebuildTrigger returns a trigger that POSTs to url, or nil if url is
// empty. A nil *rebuildTrigger is inert but safe to call — the deployment that
// has no hook configured (every dev machine, every test) pays nothing.
func newRebuildTrigger(url string) *rebuildTrigger {
	if url == "" {
		return nil
	}
	return &rebuildTrigger{post: func(ctx context.Context) error {
		return postDeployHook(ctx, url)
	}}
}

// Notify records a qualifying write. The hook fires rebuildDebounce after the
// last Notify, or rebuildMaxWait after the first of the current burst,
// whichever comes first.
func (t *rebuildTrigger) Notify() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}

	now := time.Now()
	if !t.pending {
		t.pending = true
		t.first = now
	}

	// Never push the fire past the cap measured from the first pending write.
	wait := rebuildDebounce
	if remaining := rebuildMaxWait - now.Sub(t.first); remaining < wait {
		wait = remaining
	}
	if wait < 0 {
		wait = 0
	}

	if t.timer != nil {
		t.timer.Stop()
	}
	t.timer = time.AfterFunc(wait, t.fire)
}

// fire sends one hook request and reopens the window. It runs on the timer's
// own goroutine.
func (t *rebuildTrigger) fire() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.pending = false
	t.timer = nil
	post := t.post
	t.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), rebuildPostTimeout)
	defer cancel()
	if err := post(ctx); err != nil {
		// A failed hook must never be retried into a queue. The next
		// qualifying write fires again, and a code push rebuilds regardless,
		// so the failure mode is "stale until the next change" — self-healing,
		// and strictly better than a stuck retry loop hammering a 429.
		slog.Warn("deploy hook failed", "err", err)
	} else {
		slog.Info("deploy hook fired")
	}
}

// Close stops a pending fire. Writes already coalesced into the current window
// are dropped rather than rushed out during shutdown; the next write after
// restart picks them up.
func (t *rebuildTrigger) Close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// postDeployHook sends the hook. The URL is a bearer secret — anyone holding it
// can queue builds — so it is never logged, not even on failure.
func postDeployHook(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		// Status without URL: a 429 needs to be visible (the hook is capped at
		// 10/min) but the secret must not reach the log.
		return &hookStatusError{status: resp.StatusCode}
	}
	return nil
}

type hookStatusError struct{ status int }

func (e *hookStatusError) Error() string {
	return "deploy hook returned HTTP " + http.StatusText(e.status)
}

// Wrap fingerprints the public surface around every state-changing request and
// notifies the trigger when it moved.
//
// The comparison runs after the handler has written its response, so it adds no
// latency to time-to-first-byte — and mutating requests here are rare admin and
// API writes, never read traffic, which is what makes an exact fingerprint
// affordable in the first place.
func (t *rebuildTrigger) Wrap(db *sql.DB, next http.Handler) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			next.ServeHTTP(w, r)
			return
		}

		before, err := publicFingerprint(db)
		if err != nil {
			// Without a baseline there is nothing to compare against. Serve the
			// request and skip the trigger rather than fire blindly on every
			// write until the database recovers.
			slog.Warn("rebuild fingerprint (before) failed", "err", err)
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, r)

		// Status is deliberately not consulted. A handler that mutated and then
		// failed to render still changed what the build would read, and a
		// handler that returned 200 without changing anything leaves the
		// fingerprint equal. The content is the authority, not the status line.
		after, err := publicFingerprint(db)
		if err != nil {
			slog.Warn("rebuild fingerprint (after) failed", "err", err)
			return
		}
		if before != after {
			t.Notify()
		}
	})
}

// publicFingerprint hashes exactly what the website's static build reads, and
// nothing else.
//
//   - published, untrashed entries by (slug, cid) — entryCID covers collection,
//     title, excerpt, body, tags, and published state, so any edit that reaches
//     a reader moves it, while slug catches a rename
//   - series by (slug, cid) — a body embeds series://<slug> and the build
//     splices the fragment in, so a series edit changes rendered pages
//   - collections by (slug, name, description) — these show on section pages,
//     and the table carries neither a CID nor an updated_at, so the row itself
//     is the fingerprint
//
// Drafts are absent by construction: an entry that is not published contributes
// nothing, so no amount of draft editing can move this value. Blobs are absent
// too — an upload changes no page until a body references it, and that
// reference is an entry write, which does move it.
func publicFingerprint(db *sql.DB) (string, error) {
	// Ordering is explicit in every query: SQLite's row order is not
	// contractual, and an unstable order would hash differently for identical
	// content — a phantom change, and a wasted build.
	entries, err := scanTriples(db,
		`SELECT e.slug, e.cid, '' FROM entries e
		 WHERE e.deleted_at = '' AND e.published = 1
		 ORDER BY e.slug`)
	if err != nil {
		return "", err
	}
	seriesRows, err := scanTriples(db, `SELECT slug, cid, '' FROM series ORDER BY slug`)
	if err != nil {
		return "", err
	}
	collections, err := scanTriples(db,
		`SELECT slug, name, description FROM collections ORDER BY slug`)
	if err != nil {
		return "", err
	}

	return cid.OfValue(map[string]any{
		"entries":     entries,
		"series":      seriesRows,
		"collections": collections,
	}), nil
}

// scanTriples reads a three-column string query into rows.
func scanTriples(db *sql.DB, query string) ([][3]string, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := [][3]string{}
	for rows.Next() {
		var t [3]string
		if err := rows.Scan(&t[0], &t[1], &t[2]); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
