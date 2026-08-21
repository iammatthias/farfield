package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// pending reports whether a fire is scheduled. The trigger keeps this private;
// tests reach in under the same lock the trigger uses.
func pending(tr *rebuildTrigger) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.pending
}

// countingTrigger is a trigger that records fires instead of reaching the
// network. The real POST is never exercised in tests — the URL is a secret and
// firing it queues a real build.
func countingTrigger() (*rebuildTrigger, func() int) {
	var mu sync.Mutex
	n := 0
	tr := &rebuildTrigger{post: func(context.Context) error {
		mu.Lock()
		n++
		mu.Unlock()
		return nil
	}}
	return tr, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// fingerprintAfter runs mutate and reports whether the public surface moved.
// This is the predicate the whole design rests on: fire iff the record was
// publicly visible before the mutation or is publicly visible after it.
func fingerprintAfter(t *testing.T, db *sql.DB, mutate func()) bool {
	t.Helper()
	before, err := publicFingerprint(db)
	if err != nil {
		t.Fatalf("fingerprint before: %v", err)
	}
	mutate()
	after, err := publicFingerprint(db)
	if err != nil {
		t.Fatalf("fingerprint after: %v", err)
	}
	return before != after
}

// TestFingerprintIgnoresDrafts is the constraint that justifies fingerprinting
// at all: a draft is invisible to the website, so no amount of drafting may
// spend a build. Cloudflare caps the hook at 10/min and autosave is frequent.
func TestFingerprintIgnoresDrafts(t *testing.T) {
	s, ids := readTestServer(t)

	// Editing a draft — the autosave case, over and over.
	if moved := fingerprintAfter(t, s.db, func() {
		e := &Entry{Collection: "blog", Slug: ids.draftSlug, Title: "WIP v2",
			Body: "still drafting", Published: false}
		if err := updateEntry(s.db, ids.draftSlug, e); err != nil {
			t.Fatalf("update draft: %v", err)
		}
	}); moved {
		t.Error("editing a draft moved the public fingerprint — autosaves would burn builds")
	}

	// Creating a new draft.
	if moved := fingerprintAfter(t, s.db, func() {
		if err := insertEntry(s.db, &Entry{Collection: "blog", Slug: "another-draft",
			Title: "Another", Body: "x", Published: false}); err != nil {
			t.Fatalf("insert draft: %v", err)
		}
	}); moved {
		t.Error("creating a draft moved the public fingerprint")
	}

	// Trashing a draft — it was never visible, so nothing disappears.
	if moved := fingerprintAfter(t, s.db, func() {
		if _, err := deleteEntry(s.db, "another-draft"); err != nil {
			t.Fatalf("trash draft: %v", err)
		}
	}); moved {
		t.Error("trashing a draft moved the public fingerprint")
	}
}

// TestFingerprintCatchesPublicChanges walks every transition that does change
// what a reader sees. Each must move the fingerprint, or the site goes stale.
func TestFingerprintCatchesPublicChanges(t *testing.T) {
	s, ids := readTestServer(t)

	cases := []struct {
		name   string
		mutate func()
	}{
		{"publishing a draft", func() {
			e := &Entry{Collection: "blog", Slug: ids.draftSlug, Title: "WIP",
				Body: "draft", Published: true}
			if err := updateEntry(s.db, ids.draftSlug, e); err != nil {
				t.Fatalf("publish: %v", err)
			}
		}},
		{"editing a published entry", func() {
			e := &Entry{Collection: "blog", Slug: ids.pubSlug, Title: "Hello again",
				Body: "revised", Published: true}
			if err := updateEntry(s.db, ids.pubSlug, e); err != nil {
				t.Fatalf("edit published: %v", err)
			}
		}},
		{"unpublishing", func() {
			e := &Entry{Collection: "blog", Slug: ids.pubSlug, Title: "Hello again",
				Body: "revised", Published: false}
			if err := updateEntry(s.db, ids.pubSlug, e); err != nil {
				t.Fatalf("unpublish: %v", err)
			}
		}},
		{"trashing a published entry", func() {
			// draftSlug was published by the first case above.
			if _, err := deleteEntry(s.db, ids.draftSlug); err != nil {
				t.Fatalf("trash published: %v", err)
			}
		}},
		{"restoring a published entry", func() {
			if _, err := restoreEntry(s.db, ids.draftSlug); err != nil {
				t.Fatalf("restore: %v", err)
			}
		}},
		{"adding a series", func() {
			if err := upsertSeries(s.db, &Series{Slug: "gallery", Title: "Gallery",
				Body: "![a](blob://x)"}); err != nil {
				t.Fatalf("insert series: %v", err)
			}
		}},
		{"editing a series", func() {
			if err := upsertSeries(s.db, &Series{Slug: "gallery",
				Title: "Gallery", Body: "![b](blob://y)"}); err != nil {
				t.Fatalf("update series: %v", err)
			}
		}},
		{"renaming a collection", func() {
			if _, err := updateCollection(s.db, "blog", "Journal", "notes"); err != nil {
				t.Fatalf("update collection: %v", err)
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !fingerprintAfter(t, s.db, tc.mutate) {
				t.Errorf("%s left the fingerprint unchanged — the site would stay stale", tc.name)
			}
		})
	}
}

// TestFingerprintStableAcrossReads guards the phantom-build case: computing the
// fingerprint twice with nothing in between must return the same value, or
// every write would fire regardless of what it changed.
func TestFingerprintStableAcrossReads(t *testing.T) {
	s, _ := readTestServer(t)
	if moved := fingerprintAfter(t, s.db, func() {}); moved {
		t.Error("fingerprint is unstable across identical reads — every write would fire")
	}
}

// TestMiddlewareFiresOnlyOnPublicChange checks the wiring, not the predicate:
// a mutating request that changes the public surface arms the trigger, one that
// does not leaves it idle, and a GET never fingerprints at all.
func TestMiddlewareFiresOnlyOnPublicChange(t *testing.T) {
	s, ids := readTestServer(t)
	tr, _ := countingTrigger()

	publish := func(w http.ResponseWriter, r *http.Request) {
		e := &Entry{Collection: "blog", Slug: ids.draftSlug, Title: "WIP",
			Body: "draft", Published: true}
		if err := updateEntry(s.db, ids.draftSlug, e); err != nil {
			t.Errorf("publish: %v", err)
		}
	}
	touchDraft := func(w http.ResponseWriter, r *http.Request) {
		e := &Entry{Collection: "blog", Slug: "d2", Title: "D2", Body: "x", Published: false}
		if err := insertEntry(s.db, e); err != nil {
			t.Errorf("insert draft: %v", err)
		}
	}

	// A draft write leaves the trigger idle.
	h := tr.Wrap(s.db, http.HandlerFunc(touchDraft))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/entries", nil))
	if pending(tr) {
		t.Error("a draft write armed the rebuild trigger")
	}

	// A publish arms it.
	h = tr.Wrap(s.db, http.HandlerFunc(publish))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/entries/wip", nil))
	if !pending(tr) {
		t.Error("publishing did not arm the rebuild trigger")
	}
}

// TestMiddlewareIgnoresReads keeps the fingerprint off the read path: GETs are
// the overwhelming majority of traffic and must not pay for two extra queries.
func TestMiddlewareIgnoresReads(t *testing.T) {
	s, _ := readTestServer(t)
	tr, _ := countingTrigger()

	var served bool
	h := tr.Wrap(s.db, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/entries", nil))
	if !served {
		t.Fatal("GET was not passed through")
	}
	if pending(tr) {
		t.Error("a GET armed the rebuild trigger")
	}
}

// TestNilTriggerIsInert covers every dev machine and every test: with no hook
// configured the middleware must disappear entirely rather than fingerprint
// into the void.
func TestNilTriggerIsInert(t *testing.T) {
	if got := newRebuildTrigger(""); got != nil {
		t.Fatalf("newRebuildTrigger(\"\") = %v, want nil", got)
	}
	var tr *rebuildTrigger
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if wrapped := tr.Wrap(nil, inner); wrapped == nil {
		t.Fatal("nil trigger dropped the handler")
	}
	tr.Notify() // must not panic
	tr.Close()  // must not panic
}

// TestDebounceCoalescesBurst is the sync-vault case. That command is
// all-or-nothing across the vault, so a single run can push a batch of pending
// edits back to back; they must land as one build, not one per entry.
func TestDebounceCoalescesBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr, fires := countingTrigger()
		defer tr.Close()

		const step = 2 * time.Second
		for range 12 {
			tr.Notify()
			synctest.Sleep(step) // a batch trickling in
		}
		if got := fires(); got != 0 {
			t.Fatalf("fired %d times during the burst, want 0 — the window should still be open", got)
		}

		// The last Notify was one step ago, so the window has that much less
		// left to run. Stop a second short of it, then cross it.
		synctest.Sleep(rebuildDebounce - step - time.Second)
		if got := fires(); got != 0 {
			t.Errorf("fired %d times just before the window closed, want 0", got)
		}
		synctest.Sleep(2 * time.Second)
		if got := fires(); got != 1 {
			t.Errorf("fired %d times after the window closed, want exactly 1", got)
		}
	})
}

// TestDebounceCapFires stops a long editing session from deferring publication
// forever: each save resets the 30s window, so without a cap the hook would
// never fire while someone kept working.
func TestDebounceCapFires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr, fires := countingTrigger()
		defer tr.Close()

		// A save every 10s indefinitely — the trailing window never expires
		// on its own.
		for elapsed := time.Duration(0); elapsed < rebuildMaxWait-10*time.Second; elapsed += 10 * time.Second {
			tr.Notify()
			synctest.Sleep(10 * time.Second)
		}
		if got := fires(); got != 0 {
			t.Fatalf("fired %d times before the cap, want 0", got)
		}

		tr.Notify()
		synctest.Sleep(15 * time.Second) // crosses rebuildMaxWait
		if got := fires(); got != 1 {
			t.Errorf("fired %d times at the cap, want exactly 1", got)
		}
	})
}

// TestHookFailureIsDropped: a failing hook must never retry into a queue. The
// next qualifying write fires again and a code push rebuilds regardless, so the
// failure mode is "stale until the next change" rather than a loop hammering a
// rate limit.
func TestHookFailureIsDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		attempts := 0
		tr := &rebuildTrigger{post: func(context.Context) error {
			mu.Lock()
			attempts++
			mu.Unlock()
			return &hookStatusError{status: http.StatusTooManyRequests}
		}}
		defer tr.Close()

		tr.Notify()
		synctest.Sleep(rebuildDebounce + time.Second)
		synctest.Sleep(2 * rebuildMaxWait) // ample room for a retry to show up

		mu.Lock()
		defer mu.Unlock()
		if attempts != 1 {
			t.Errorf("hook attempted %d times, want exactly 1 — a failure must not retry", attempts)
		}
	})
}

// TestCloseCancelsPendingFire: shutdown must not race a POST out the door.
func TestCloseCancelsPendingFire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr, fires := countingTrigger()
		tr.Notify()
		tr.Close()
		synctest.Sleep(rebuildDebounce + time.Second)
		if got := fires(); got != 0 {
			t.Errorf("fired %d times after Close, want 0", got)
		}
	})
}
